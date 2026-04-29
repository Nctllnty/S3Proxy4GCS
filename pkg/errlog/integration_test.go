package errlog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd_FileSinkWritesErrorOnly spins up a real ymlog file writer
// in a temp dir, sends one Info + one Error record through the wrapped
// slog handler, then asserts only the error line lands in the file and
// the line shape matches the documented format:
//
//	time="2006-01-02T15:04:05Z" level=error msg="..." key="value"
func TestEndToEnd_FileSinkWritesErrorOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error_%Y%M%D.log")

	Init(path, 0, 1, 16)
	t.Cleanup(func() { defaultLogger.Store(nil) })

	logger := slog.New(NewHandler(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))

	logger.Info("info-not-mirrored", "key", "v")
	logger.Error("GCS API call failed", "error", "404 NotFound", "status", 404)

	// ymlog flushes asynchronously via a goroutine; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	now := time.Now().UTC()
	expected := filepath.Join(dir, "error_"+now.Format("20060102")+".log")
	var data []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(expected)
		if err == nil && len(b) > 0 {
			data = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("error log file %q never received data", expected)
	}

	content := string(data)
	if strings.Contains(content, "info-not-mirrored") {
		t.Errorf("info record leaked into error log:\n%s", content)
	}
	if !strings.Contains(content, `level=error`) {
		t.Errorf("expected level=error, got:\n%s", content)
	}
	if !strings.Contains(content, `msg="GCS API call failed"`) {
		t.Errorf("expected quoted msg, got:\n%s", content)
	}
	if !strings.Contains(content, `error="404 NotFound"`) || !strings.Contains(content, `status="404"`) {
		t.Errorf("expected attrs in line, got:\n%s", content)
	}
	if !strings.HasPrefix(content, `time="`) {
		t.Errorf("expected line to start with time=, got:\n%s", content)
	}
}
