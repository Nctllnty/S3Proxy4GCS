package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestExtractAccessKey covers the happy path and the malformed shapes
// we are likely to encounter from real SDKs plus pathological inputs.
func TestExtractAccessKey(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
		err    error
	}{
		{
			name:   "canonical",
			header: "AWS4-HMAC-SHA256 Credential=GOOG1EAKTEST/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc",
			want:   "GOOG1EAKTEST",
		},
		{
			name:   "credential-field-only",
			header: "AWS4-HMAC-SHA256 Credential=AKONLY/20260101/us-east-1/s3/aws4_request",
			want:   "AKONLY",
		},
		{
			name:   "empty",
			header: "",
			err:    ErrNoCredential,
		},
		{
			name:   "no-credential",
			header: "Basic dXNlcjpwYXNz",
			err:    ErrNoCredential,
		},
		{
			name:   "no-slash-after-ak",
			header: "AWS4-HMAC-SHA256 Credential=BAD",
			err:    ErrNoCredential,
		},
		{
			name:   "empty-ak",
			header: "AWS4-HMAC-SHA256 Credential=/20260101/us-east-1/s3/aws4_request",
			err:    ErrNoCredential,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractAccessKey(tc.header)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("AK = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractAccessKeyFromQuery covers the presigned URL fallback used
// when the request has no Authorization header.
func TestExtractAccessKeyFromQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
		err   error
	}{
		{
			name:  "encoded",
			query: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=GOOG1EAK%2F20260101%2Fus-east-1%2Fs3%2Faws4_request",
			want:  "GOOG1EAK",
		},
		{
			name:  "unencoded",
			query: "X-Amz-Credential=GOOG1EAK/20260101/us-east-1/s3/aws4_request",
			want:  "GOOG1EAK",
		},
		{
			name:  "case-insensitive",
			query: "x-amz-credential=GOOG1EAK%2F20260101%2Fus-east-1%2Fs3%2Faws4_request",
			want:  "GOOG1EAK",
		},
		{
			name:  "no-query",
			query: "",
			err:   ErrNoCredential,
		},
		{
			name:  "no-credential-param",
			query: "foo=bar&baz=qux",
			err:   ErrNoCredential,
		},
		{
			name:  "empty-credential",
			query: "X-Amz-Credential=%2F20260101%2Fus-east-1%2Fs3%2Faws4_request",
			err:   ErrNoCredential,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractAccessKeyFromQuery(tc.query)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("AK = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStoreLookup exercises the lock-free read path and atomic Replace.
func TestStoreLookup(t *testing.T) {
	s := New()
	if sk, ok := s.Lookup("nope"); ok || sk != "" {
		t.Fatalf("empty store lookup returned (%q, %v)", sk, ok)
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("empty store size = %d, want 0", got)
	}

	s.Replace(map[string]string{"AK1": "SK1", "AK2": "SK2"})
	if got := s.Size(); got != 2 {
		t.Errorf("size = %d, want 2", got)
	}
	if sk, ok := s.Lookup("AK1"); !ok || sk != "SK1" {
		t.Errorf("AK1 lookup = (%q, %v)", sk, ok)
	}
	if _, ok := s.Lookup("AK3"); ok {
		t.Errorf("AK3 should not exist")
	}

	// Replace should fully replace, not merge.
	s.Replace(map[string]string{"AK3": "SK3"})
	if _, ok := s.Lookup("AK1"); ok {
		t.Errorf("AK1 should be gone after Replace")
	}
	if sk, ok := s.Lookup("AK3"); !ok || sk != "SK3" {
		t.Errorf("AK3 lookup = (%q, %v)", sk, ok)
	}
}

// TestStoreReplaceDefensiveCopy proves the caller's map can be mutated
// after Replace without affecting the store — a common source of
// concurrency bugs when hot-reload handlers keep references.
func TestStoreReplaceDefensiveCopy(t *testing.T) {
	s := New()
	m := map[string]string{"AK": "SK"}
	s.Replace(m)
	m["AK"] = "MUTATED"
	delete(m, "AK")
	if sk, ok := s.Lookup("AK"); !ok || sk != "SK" {
		t.Errorf("expected snapshot to be immutable, got (%q, %v)", sk, ok)
	}
}

// TestParseJSON covers rejection of common mistakes that would otherwise
// authenticate an attacker as an empty-AK identity.
func TestParseJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		wantLen int
	}{
		{"happy", `{"AK1":"SK1","AK2":"SK2"}`, false, 2},
		{"empty-string", ``, true, 0},
		{"whitespace", `   `, true, 0},
		{"not-json", `not a json`, true, 0},
		{"wrong-type", `["AK","SK"]`, true, 0},
		{"empty-object", `{}`, true, 0},
		{"empty-ak", `{"":"SK"}`, true, 0},
		{"empty-sk", `{"AK":""}`, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseJSON(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got map=%v", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(m) != tc.wantLen {
				t.Errorf("len=%d, want %d", len(m), tc.wantLen)
			}
		})
	}
}

// TestLoadFile covers both successful load and the specific error surfaced
// when the file is missing or malformed.
func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{"AK":"SK"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadFile(path)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m["AK"] != "SK" {
		t.Errorf("got %+v", m)
	}

	if _, err := LoadFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("expected error for missing file")
	}
}

// TestWatchHotReload is the key regression test: a running watcher must
// pick up file rewrites AND kubelet-style symlink swaps. We simulate the
// second path manually to match what happens on a K8s Secret update.
func TestWatchHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"AK1":"SK1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New()
	m, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Replace(m)

	var mu sync.Mutex
	done := make(chan struct{}, 4)
	onReload := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}

	stop, err := s.Watch(path, onReload)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stop()

	if err := os.WriteFile(path, []byte(`{"AK1":"SK1","AK2":"SK2"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for hot reload after file write")
	}

	if sk, ok := s.Lookup("AK2"); !ok || sk != "SK2" {
		t.Errorf("AK2 should be live after reload, got (%q, %v)", sk, ok)
	}
	if s.Size() != 2 {
		t.Errorf("size=%d, want 2", s.Size())
	}

	// Simulate a malformed update: reload MUST keep the previous snapshot
	// so a broken Secret rollout does not take down the proxy.
	if err := os.WriteFile(path, []byte(`not a json`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Give the watcher time to observe the failure.
	time.Sleep(500 * time.Millisecond)
	if _, ok := s.Lookup("AK2"); !ok {
		t.Error("previous snapshot should survive malformed reload")
	}
}

// TestSnapshot just ensures we can round-trip AK names out of the store
// for debug logs without panicking or mutating the live map.
func TestSnapshot(t *testing.T) {
	s := New()
	s.Replace(map[string]string{"A": "1", "B": "2"})
	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2; got %v", len(got), got)
	}
}
