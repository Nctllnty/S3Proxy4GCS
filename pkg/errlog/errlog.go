// Package errlog writes ERROR-level slog records to a dedicated local file
// (e.g. ./logs/error_YYYYMMDD.log) using the same ymlog.FileLoggerWriter
// machinery as pkg/reqlog. It is purely additive: the wrapped slog handler
// continues to forward every record (Debug/Info/Warn/Error) to its inner
// destination (typically the JSON handler bound to os.Stderr), and only
// errors are mirrored to the file in a logfmt-style line:
//
//	time="2026-04-29T02:51:01Z" level=error msg="..." key1="v1" key2="v2"
//
// The file logger is asynchronous (channel-buffered) and non-blocking on
// the hot path; if Init has not been called the SlogHandler degrades to
// the inner handler only, so packages can construct the handler chain
// before knowing whether the file sink is enabled.
package errlog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maczam/ymlog"
)

// defaultLogger is the package-level singleton populated by Init().
// Stored in an atomic.Value so the SlogHandler can read it lock-free on
// every error record. nil means "file sink disabled".
var defaultLogger atomic.Pointer[ymlog.Logger]

// Init initializes the underlying ymlog.Logger with a FileLoggerWriter.
// filePath supports %Y%M%D date patterns (e.g. "./logs/error_%Y%M%D.log").
// maxSizeMB is the per-file size limit in megabytes (0 = no limit).
// Call once from main() before the HTTP server starts.
func Init(filePath string, maxSizeMB, maxBackup, chanBuf int) {
	logger := ymlog.NewLogger(&ymlog.FileLoggerWriter{
		FileName:         filePath,
		RotateDaily:      true,
		RotateSize:       maxSizeMB > 0,
		MaxSize:          maxSizeMB,
		MaxBackup:        maxBackup,
		ChanBufferLength: chanBuf,
		WriteFileBuffer:  5,
	})
	defaultLogger.Store(logger)
}

// writeLine emits a pre-formatted line to the file sink. No-op when Init
// has not been called.
func writeLine(line string) {
	if l := defaultLogger.Load(); l != nil {
		l.InfoString(line)
	}
}

// SlogHandler wraps an inner slog.Handler and mirrors every record whose
// Level >= LevelError to the local error log file. Non-error records are
// passed through unchanged so the existing stderr JSON stream stays intact.
type SlogHandler struct {
	inner slog.Handler
	// attrs / group state captured by With/WithGroup so they can be
	// re-applied when formatting the file line. The inner handler keeps
	// its own copy via inner.WithAttrs / WithGroup.
	attrs  []slog.Attr
	groups []string
}

// NewHandler wraps inner so that ERROR-level records are also written to
// the file sink configured by Init. inner MUST not be nil.
func NewHandler(inner slog.Handler) *SlogHandler {
	return &SlogHandler{inner: inner}
}

// Enabled defers to the inner handler — the file sink only fires for
// errors but we never want to mask the inner stream's own filtering.
func (h *SlogHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

// Handle forwards the record to the inner handler and, when the level is
// ERROR or above, also writes a logfmt-style line to the file sink.
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && defaultLogger.Load() != nil {
		writeLine(formatRecord(r, h.attrs, h.groups))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a new handler with attrs appended to both the inner
// handler and the file-side attribute snapshot.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &SlogHandler{
		inner:  h.inner.WithAttrs(attrs),
		attrs:  merged,
		groups: h.groups,
	}
}

// WithGroup tracks the group name for the file-side formatter and forwards
// to the inner handler.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &SlogHandler{
		inner:  h.inner.WithGroup(name),
		attrs:  h.attrs,
		groups: append(append([]string{}, h.groups...), name),
	}
}

// formatRecord renders a slog.Record as:
//
//	time="2006-01-02T15:04:05Z" level=error msg="..." k1="v1" k2="v2"
//
// Quotes inside string values are backslash-escaped. UTC is used so logs
// from multiple replicas line up regardless of the host time zone.
func formatRecord(r slog.Record, baseAttrs []slog.Attr, groups []string) string {
	var b strings.Builder
	b.Grow(128)

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	b.WriteString(`time="`)
	b.WriteString(ts.UTC().Format("2006-01-02T15:04:05Z"))
	b.WriteString(`" level=`)
	b.WriteString(strings.ToLower(r.Level.String()))
	b.WriteString(` msg=`)
	b.WriteString(strconv.Quote(r.Message))

	groupPrefix := strings.Join(groups, ".")
	if groupPrefix != "" {
		groupPrefix += "."
	}

	for _, a := range baseAttrs {
		appendAttr(&b, groupPrefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, groupPrefix, a)
		return true
	})
	return b.String()
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		groupAttrs := a.Value.Group()
		if len(groupAttrs) == 0 {
			return
		}
		nestedPrefix := prefix
		if a.Key != "" {
			nestedPrefix += a.Key + "."
		}
		for _, ga := range groupAttrs {
			appendAttr(b, nestedPrefix, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(strconv.Quote(formatValue(a.Value)))
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", v.Any())
	}
}

// StartCleanup launches a background goroutine that runs once daily at
// 00:05 and deletes error_YYYYMMDD.log files (and rotation suffixes
// .1 / .2 / …) whose date is older than keepDays days ago. Mirrors the
// rotation/cleanup contract used by pkg/reqlog so operators only have to
// learn one model.
func StartCleanup(dir string, keepDays int) {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, now.Location())
			time.Sleep(time.Until(next))
			cleanOldLogs(dir, keepDays)
		}
	}()
}

var logFileRe = regexp.MustCompile(`^error_(\d{8})\.log(\.\d+)?$`)

func cleanOldLogs(dir string, keepDays int) {
	cutoff := time.Now().AddDate(0, 0, -keepDays)

	files, err := os.ReadDir(dir)
	if err != nil {
		// Use stderr directly to avoid recursing through the slog handler
		// chain (which would re-enter writeLine).
		fmt.Fprintf(os.Stderr, "errlog cleanup: read dir failed dir=%s error=%v\n", dir, err)
		return
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		m := logFileRe.FindStringSubmatch(f.Name())
		if len(m) < 2 {
			continue
		}
		fileDate, err := time.Parse("20060102", m[1])
		if err != nil {
			continue
		}
		if !fileDate.After(cutoff) {
			fullPath := filepath.Join(dir, f.Name())
			if err := os.Remove(fullPath); err != nil {
				fmt.Fprintf(os.Stderr, "errlog cleanup: remove failed path=%s error=%v\n", fullPath, err)
			}
		}
	}
}
