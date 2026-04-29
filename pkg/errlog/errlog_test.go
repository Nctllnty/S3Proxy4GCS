package errlog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fixedTime is the timestamp used by every test record so the formatted
// line is deterministic.
var fixedTime = time.Date(2026, 4, 29, 2, 51, 1, 0, time.UTC)

func newRecord(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(fixedTime, level, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

func TestFormatRecord_BasicError(t *testing.T) {
	r := newRecord(slog.LevelError, "GCS API call failed",
		slog.String("error", "404 NotFound"),
		slog.Int("status", 404),
	)

	got := formatRecord(r, nil, nil)
	want := `time="2026-04-29T02:51:01Z" level=error msg="GCS API call failed" error="404 NotFound" status="404"`
	if got != want {
		t.Errorf("format mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestFormatRecord_NoAttrs(t *testing.T) {
	r := newRecord(slog.LevelError, "boom")
	got := formatRecord(r, nil, nil)
	want := `time="2026-04-29T02:51:01Z" level=error msg="boom"`
	if got != want {
		t.Errorf("format mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestFormatRecord_QuotesEscaped(t *testing.T) {
	r := newRecord(slog.LevelError, `quote " inside`,
		slog.String("path", `/tmp/"odd"/file`),
	)
	got := formatRecord(r, nil, nil)
	if !strings.Contains(got, `msg="quote \" inside"`) {
		t.Errorf("msg quotes not escaped: %s", got)
	}
	if !strings.Contains(got, `path="/tmp/\"odd\"/file"`) {
		t.Errorf("attr quotes not escaped: %s", got)
	}
}

func TestFormatRecord_BaseAttrsAndGroup(t *testing.T) {
	r := newRecord(slog.LevelError, "hi", slog.String("k", "v"))
	got := formatRecord(r, []slog.Attr{slog.String("request_id", "abc123")}, []string{"http"})
	want := `time="2026-04-29T02:51:01Z" level=error msg="hi" http.request_id="abc123" http.k="v"`
	if got != want {
		t.Errorf("format mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// captureHandler is a minimal slog.Handler that records every record it
// receives so we can verify pass-through behavior.
type captureHandler struct {
	records []slog.Record
	buf     bytes.Buffer
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.records = append(c.records, r)
	c.buf.WriteString(r.Message)
	c.buf.WriteByte('\n')
	return nil
}
func (c *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler            { return c }

func TestSlogHandler_PassThroughAlways(t *testing.T) {
	cap := &captureHandler{}
	h := NewHandler(cap)
	ctx := context.Background()

	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if err := h.Handle(ctx, newRecord(lvl, lvl.String())); err != nil {
			t.Fatalf("Handle(%s) error: %v", lvl, err)
		}
	}
	if len(cap.records) != 4 {
		t.Fatalf("expected inner handler to receive 4 records, got %d", len(cap.records))
	}
}

func TestSlogHandler_FileSinkOnlyOnError(t *testing.T) {
	defaultLogger.Store(nil)
	cap := &captureHandler{}
	h := NewHandler(cap)
	ctx := context.Background()

	if err := h.Handle(ctx, newRecord(slog.LevelInfo, "info-msg")); err != nil {
		t.Fatalf("Handle(info) error: %v", err)
	}
	if err := h.Handle(ctx, newRecord(slog.LevelError, "err-msg")); err != nil {
		t.Fatalf("Handle(error) error: %v", err)
	}

	if got := len(cap.records); got != 2 {
		t.Fatalf("expected 2 inner records, got %d", got)
	}
}

func TestSlogHandler_WithAttrsCarriesIntoFileFormatter(t *testing.T) {
	defaultLogger.Store(nil)
	cap := &captureHandler{}
	h := NewHandler(cap).WithAttrs([]slog.Attr{slog.String("request_id", "xyz")})

	r := newRecord(slog.LevelError, "fail", slog.Int("status", 500))
	got := formatRecord(r, h.(*SlogHandler).attrs, h.(*SlogHandler).groups)
	want := `time="2026-04-29T02:51:01Z" level=error msg="fail" request_id="xyz" status="500"`
	if got != want {
		t.Errorf("format mismatch:\n got: %s\nwant: %s", got, want)
	}
}
