// Package credstore manages the per-client HMAC credential mapping used
// by the Director to re-sign each inbound S3 request with the client's own
// AK/SK. The mapping replaces the single proxy-wide key pair that v1.6 and
// earlier used (see docs/hmac-credential-mapping-design.md).
//
// Design goals:
//   - Zero-allocation, lock-free read path (Lookup) using atomic.Value so
//     per-request credential resolution does not contend with reloads.
//   - Hot reload: Replace() swaps the whole map atomically; Watch() drives
//     Replace() from file-system events so K8s Secret volume updates take
//     effect without restarting the proxy.
//   - Strict input validation so a typo in the credential file cannot
//     silently authorise every caller as an empty-AK identity.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"s3proxy4gcs/pkg/metrics"
)

// ErrNoCredential is returned by ExtractAccessKey when the SigV4
// Authorization header is missing or malformed.
var ErrNoCredential = errors.New("no SigV4 credential in Authorization header")

// Store holds an immutable map[AK]SK behind an atomic.Value so Lookup()
// can run on the request hot path without taking a lock.
//
// Replace() swaps the entire map; callers must not retain references to
// the map they passed in. Size() reports the current entry count for the
// startup log and tests.
type Store struct {
	v atomic.Value // map[string]string
}

// New returns an empty store. Use Replace to populate it before serving
// requests; Lookup on an empty store always returns (ok=false).
func New() *Store {
	s := &Store{}
	s.v.Store(map[string]string{})
	return s
}

// Replace atomically swaps the backing map. A defensive copy is taken so
// the caller can mutate or discard their map immediately afterwards. The
// `s3proxy_hmac_credentials_loaded` gauge is updated to match the new
// size.
func (s *Store) Replace(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	s.v.Store(cp)
	metrics.HMACCredentialsLoaded.Set(float64(len(cp)))
}

// Lookup returns the secret key for an access key ID. The second return
// is false when the AK is not in the map.
func (s *Store) Lookup(ak string) (string, bool) {
	m := s.v.Load().(map[string]string)
	sk, ok := m[ak]
	return sk, ok
}

// Size reports how many entries the store currently holds.
func (s *Store) Size() int {
	m := s.v.Load().(map[string]string)
	return len(m)
}

// Snapshot returns a copy of the current access-key set. Intended for
// debug logs and tests; production code paths should call Lookup.
func (s *Store) Snapshot() []string {
	m := s.v.Load().(map[string]string)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// LoadFile reads a JSON `{ "<AK>": "<SK>", ... }` file and returns the
// parsed map. The returned error has a human-readable prefix so the
// startup log points directly at the misconfiguration.
func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials file %q: %w", path, err)
	}
	return parse(data)
}

// ParseJSON parses a JSON credential map from a raw string. Used by the
// HMAC_CREDENTIALS env var loader in config/settings.go.
func ParseJSON(raw string) (map[string]string, error) {
	return parse([]byte(raw))
}

func parse(data []byte) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("credentials payload is empty")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, fmt.Errorf("parse credentials JSON: %w", err)
	}
	if len(m) == 0 {
		return nil, errors.New("credentials JSON is an empty object")
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("credentials JSON contains an empty access key id")
		}
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("credentials JSON contains an empty secret for AK %q", k)
		}
	}
	return m, nil
}

// ExtractAccessKey returns the access key id embedded in a SigV4
// Authorization header. The header format is fixed:
//
//	AWS4-HMAC-SHA256 Credential=<AK>/<date>/<region>/<service>/aws4_request, SignedHeaders=..., Signature=...
//
// so we parse by string slicing rather than going through url.Values or
// a regexp. Returns ErrNoCredential if no Credential= field is present
// (e.g. anonymous requests or presigned requests using query params —
// callers that need query-string AK support should fall back to
// ExtractAccessKeyFromQuery).
func ExtractAccessKey(header string) (string, error) {
	if header == "" {
		return "", ErrNoCredential
	}
	idx := strings.Index(header, "Credential=")
	if idx < 0 {
		return "", ErrNoCredential
	}
	rest := header[idx+len("Credential="):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", ErrNoCredential
	}
	return rest[:slash], nil
}

// ExtractAccessKeyFromQuery returns the access key id from an S3
// presigned URL (`X-Amz-Credential=<AK>/<date>/...`). Used as a fallback
// when the header-based parser returns ErrNoCredential.
func ExtractAccessKeyFromQuery(query string) (string, error) {
	if query == "" {
		return "", ErrNoCredential
	}
	// Tiny hand-rolled parser to avoid url.ParseQuery allocations on the
	// hot path; the key name is case-insensitive in the SigV4 spec.
	for len(query) > 0 {
		seg := query
		if i := strings.IndexByte(seg, '&'); i >= 0 {
			seg = query[:i]
			query = query[i+1:]
		} else {
			query = ""
		}
		eq := strings.IndexByte(seg, '=')
		if eq < 0 {
			continue
		}
		name := seg[:eq]
		if !strings.EqualFold(name, "X-Amz-Credential") {
			continue
		}
		val := seg[eq+1:]
		// Presigned URLs URL-encode the "/" as "%2F".
		val = strings.ReplaceAll(val, "%2F", "/")
		val = strings.ReplaceAll(val, "%2f", "/")
		slash := strings.IndexByte(val, '/')
		if slash <= 0 {
			return "", ErrNoCredential
		}
		return val[:slash], nil
	}
	return "", ErrNoCredential
}

// Watch starts a goroutine that monitors the directory containing `path`
// and calls Replace() whenever the credential file changes.
//
// Kubernetes updates Secret volume mounts via an atomic rename of the
// `..data` symlink, not by writing the real file in place. fsnotify on
// the file itself would lose the reference after the first rename, so
// we watch the parent directory and filter events manually.
//
// onReload is invoked after each reload attempt (nil err on success)
// and is intended for tests. Production callers can pass nil.
//
// The watcher stops when the returned `stop` function is called; it is
// also automatically cancelled when the program exits.
func (s *Store) Watch(path string, onReload func(error)) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}
	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch directory %q: %w", dir, err)
	}
	done := make(chan struct{})
	go s.runWatcher(watcher, path, onReload, done)
	return func() {
		select {
		case <-done:
			return
		default:
		}
		close(done)
		watcher.Close()
	}, nil
}

// debounceDelay is the coalescing window for burst fsnotify events. K8s
// rewrites the `..data` symlink in several stages (CREATE → RENAME →
// REMOVE old); without debouncing we would reload 3-4 times in a row.
const debounceDelay = 200 * time.Millisecond

func (s *Store) runWatcher(watcher *fsnotify.Watcher, path string, onReload func(error), done <-chan struct{}) {
	base := filepath.Base(path)
	var pending <-chan time.Time
	for {
		select {
		case <-done:
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			evBase := filepath.Base(ev.Name)
			// K8s Secret updates touch the symlink target ("..data"); plain file
			// rewrites touch the real file name. Accept either so behaviour is
			// identical in cluster and in local dev.
			if evBase != base && evBase != "..data" {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			pending = time.After(debounceDelay)
		case <-pending:
			pending = nil
			m, err := LoadFile(path)
			if err != nil {
				slog.Error("HMAC credentials hot-reload failed; keeping previous map",
					"path", path, "error", err)
				metrics.HMACCredentialsReloadTotal.WithLabelValues("error").Inc()
				if onReload != nil {
					onReload(err)
				}
				continue
			}
			s.Replace(m)
			slog.Info("HMAC credentials hot-reloaded", "path", path, "count", len(m))
			metrics.HMACCredentialsReloadTotal.WithLabelValues("success").Inc()
			if onReload != nil {
				onReload(nil)
			}
		case werr, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("fsnotify watcher error", "path", path, "error", werr)
		}
	}
}
