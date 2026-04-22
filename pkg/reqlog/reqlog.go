package reqlog

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	chiMW "github.com/go-chi/chi/v5/middleware"
	"github.com/maczam/ymlog"
)

// Default is the package-level singleton logger, initialized by Init().
var Default *ymlog.Logger

// Init initializes the ymlog.Logger with a FileLoggerWriter.
// filePath supports %Y%M%D date patterns (e.g. "./logs/req_%Y%M%D.csv").
// maxSizeMB is the per-file size limit in megabytes (0 = no limit).
// Call once from main() before the HTTP server starts.
func Init(filePath string, maxSizeMB, maxBackup, chanBuf int) {
	Default = ymlog.NewLogger(&ymlog.FileLoggerWriter{
		FileName:         filePath,
		RotateDaily:      true,
		RotateSize:       maxSizeMB > 0,
		MaxSize:          maxSizeMB,
		MaxBackup:        maxBackup,
		ChanBufferLength: chanBuf,
		WriteFileBuffer:  5,
	})
}

// Record holds the fields logged for each request.
type Record struct {
	TimestampMs       int64
	RequestID         string
	SourceIP          string
	HTTPMethod        string
	APIMethod         string
	Bucket            string
	AccessKey         string
	GUploaderUploadID string
	StatusCode        int
	DurationMs        int64
}

// ToCSVLine serializes the record as a SOH-delimited (\u0001) line.
func (rec Record) ToCSVLine() string {
	return strings.Join([]string{
		strconv.FormatInt(rec.TimestampMs, 10),
		rec.RequestID,
		rec.SourceIP,
		rec.HTTPMethod,
		rec.APIMethod,
		rec.Bucket,
		rec.AccessKey,
		rec.GUploaderUploadID,
		strconv.Itoa(rec.StatusCode),
		strconv.FormatInt(rec.DurationMs, 10),
	}, "\u0001")
}

// statusRecorder wraps http.ResponseWriter to capture the response status code.
//
// Unwrap() and Flush() are exposed so http.ResponseController (used by
// httputil.ReverseProxy for streaming flush) can reach the real writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController traverse to the real ResponseWriter.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// Flush proxies to the underlying writer's Flusher for streaming responses.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware returns a Chi-compatible middleware that logs each request to logger
// in SOH-delimited CSV format. It requires middleware.RequestID to be applied first.
func Middleware(logger *ymlog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			entry := Record{
				TimestampMs:       start.UnixMilli(),
				RequestID:         chiMW.GetReqID(r.Context()),
				SourceIP:          ExtractSourceIP(r),
				HTTPMethod:        r.Method,
				APIMethod:         InferAPIMethod(r),
				Bucket:            ExtractBucket(r),
				AccessKey:         ExtractAccessKey(r),
				GUploaderUploadID: ExtractGUploaderUploadID(rec.Header()),
				StatusCode:        rec.status,
				DurationMs:        time.Since(start).Milliseconds(),
			}
			logger.InfoString(entry.ToCSVLine())
		})
	}
}

// ExtractGUploaderUploadID returns the GCS upload trace ID from the response headers.
func ExtractGUploaderUploadID(h http.Header) string {
	return h.Get("X-GUploader-UploadID")
}

// ExtractSourceIP returns the client IP from X-Forwarded-For (first value),
// then X-Real-Ip, then r.RemoteAddr.
func ExtractSourceIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// InferAPIMethod maps the HTTP method + path + query parameters to an S3 operation name.
func InferAPIMethod(r *http.Request) string {
	method := r.Method
	path := r.URL.Path
	q := r.URL.Query()

	hasQuery := func(key string) bool {
		_, ok := q[key]
		return ok
	}

	// Root path = ListBuckets
	if path == "/" {
		if method == http.MethodGet {
			return "ListBuckets"
		}
		return "Unknown"
	}

	// Control-plane query-param operations
	if hasQuery("lifecycle") {
		switch method {
		case http.MethodGet:
			return "GetBucketLifecycle"
		case http.MethodPut:
			return "PutBucketLifecycle"
		case http.MethodDelete:
			return "DeleteBucketLifecycle"
		}
	}
	if hasQuery("cors") {
		switch method {
		case http.MethodGet:
			return "GetBucketCors"
		case http.MethodPut:
			return "PutBucketCors"
		case http.MethodDelete:
			return "DeleteBucketCors"
		}
	}
	if hasQuery("acl") {
		switch method {
		case http.MethodGet:
			return "GetObjectAcl"
		case http.MethodPut:
			return "PutObjectAcl"
		}
	}
	if hasQuery("versioning") {
		switch method {
		case http.MethodGet:
			return "GetBucketVersioning"
		case http.MethodPut:
			return "PutBucketVersioning"
		}
	}
	if hasQuery("tagging") {
		switch method {
		case http.MethodGet:
			return "GetTagging"
		case http.MethodPut:
			return "PutTagging"
		case http.MethodDelete:
			return "DeleteTagging"
		}
	}
	if hasQuery("delete") {
		return "DeleteObjects"
	}

	// Determine bucket and key from path
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	hasBucket := len(parts) >= 1 && parts[0] != ""
	hasKey := len(parts) == 2 && parts[1] != ""

	if hasBucket && !hasKey {
		switch method {
		case http.MethodGet:
			return "ListObjects"
		case http.MethodHead:
			return "HeadBucket"
		case http.MethodPut:
			return "CreateBucket"
		case http.MethodDelete:
			return "DeleteBucket"
		}
	}
	if hasBucket && hasKey {
		switch method {
		case http.MethodGet:
			return "GetObject"
		case http.MethodPut:
			return "PutObject"
		case http.MethodDelete:
			return "DeleteObject"
		case http.MethodHead:
			return "HeadObject"
		}
	}

	return "Unknown"
}

// ExtractBucket returns the bucket name from the request.
// Supports path-style (/bucket/key) only; virtual-hosted style is not applicable
// in this proxy since all requests arrive at a single host.
func ExtractBucket(r *http.Request) string {
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	return parts[0]
}

// credentialRe matches the Access Key ID inside SigV4 "Credential=AK/date/region/service/aws4_request".
// It tolerates optional whitespace after "Credential=" and stops at the first "/".
var credentialRe = regexp.MustCompile(`Credential=\s*([^/,\s]+)/`)

// ExtractAccessKey returns the Access Key ID used to sign the request.
//
// Supported signing schemes:
//   - AWS Signature V4 via Authorization header:
//     "AWS4-HMAC-SHA256 Credential=AKID/YYYYMMDD/region/s3/aws4_request, SignedHeaders=..., Signature=..."
//   - AWS Signature V4 via presigned URL query:
//     ?X-Amz-Credential=AKID%2FYYYYMMDD%2Fregion%2Fs3%2Faws4_request
//   - AWS Signature V2 via Authorization header:
//     "AWS AKID:signature"
//
// Returns "" if no recognizable Access Key ID is found (e.g. anonymous requests).
func ExtractAccessKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if m := credentialRe.FindStringSubmatch(auth); len(m) == 2 {
			return m[1]
		}
		// Signature V2: "AWS AKID:signature"
		if rest, ok := strings.CutPrefix(auth, "AWS "); ok {
			if i := strings.IndexByte(rest, ':'); i > 0 {
				return strings.TrimSpace(rest[:i])
			}
		}
	}
	// Presigned URL (SigV4). URL.Query() already percent-decodes the value,
	// so the raw form "AKID/date/region/service/aws4_request" is restored.
	if cred := r.URL.Query().Get("X-Amz-Credential"); cred != "" {
		if i := strings.IndexByte(cred, '/'); i > 0 {
			return cred[:i]
		}
	}
	// Presigned URL (SigV2).
	if ak := r.URL.Query().Get("AWSAccessKeyId"); ak != "" {
		return ak
	}
	return ""
}

// StartCleanup launches a background goroutine that runs once daily at 00:05,
// deleting req_YYYYMMDD.csv files (including rotation suffixes .1, .2 …)
// whose date is older than keepDays days ago.
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

var logFileRe = regexp.MustCompile(`^req_(\d{8})\.csv(\.\d+)?$`)

func cleanOldLogs(dir string, keepDays int) {
	cutoff := time.Now().AddDate(0, 0, -keepDays)

	files, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("reqlog cleanup: read dir failed", "dir", dir, "error", err)
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
				slog.Error("reqlog cleanup: remove failed", "path", fullPath, "error", err)
			} else {
				slog.Info("reqlog cleanup: removed", "path", fullPath)
			}
		}
	}
}
