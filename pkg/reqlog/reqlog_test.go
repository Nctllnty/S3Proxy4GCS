package reqlog

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractAccessKey(t *testing.T) {
	tests := []struct {
		name    string
		build   func() *http.Request
		wantKey string
	}{
		{
			name: "SigV4 Authorization header",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
				r.Header.Set("Authorization",
					"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20240421/us-east-1/s3/aws4_request, "+
						"SignedHeaders=host;x-amz-date, "+
						"Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024")
				return r
			},
			wantKey: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name: "SigV4 Authorization header with extra whitespace",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
				r.Header.Set("Authorization",
					"AWS4-HMAC-SHA256 Credential= AKIDSPACE/20240421/us-east-1/s3/aws4_request, "+
						"SignedHeaders=host, Signature=deadbeef")
				return r
			},
			wantKey: "AKIDSPACE",
		},
		{
			name: "SigV4 presigned URL (percent-encoded)",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet,
					"/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256"+
						"&X-Amz-Credential=AKIAPRESIGNED123%2F20240421%2Fus-east-1%2Fs3%2Faws4_request"+
						"&X-Amz-Date=20240421T000000Z&X-Amz-Expires=3600"+
						"&X-Amz-SignedHeaders=host&X-Amz-Signature=abc",
					nil)
			},
			wantKey: "AKIAPRESIGNED123",
		},
		{
			name: "SigV2 Authorization header",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
				r.Header.Set("Authorization", "AWS AKIAV2EXAMPLE:bWFnaWNzaWc=")
				return r
			},
			wantKey: "AKIAV2EXAMPLE",
		},
		{
			name: "SigV2 presigned URL",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet,
					"/bucket/key?AWSAccessKeyId=AKIAV2QUERY&Signature=xyz&Expires=1700000000",
					nil)
			},
			wantKey: "AKIAV2QUERY",
		},
		{
			name: "Anonymous request",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
			},
			wantKey: "",
		},
		{
			name: "Malformed Authorization header",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
				r.Header.Set("Authorization", "Bearer some-jwt-token")
				return r
			},
			wantKey: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractAccessKey(tt.build())
			if got != tt.wantKey {
				t.Errorf("ExtractAccessKey() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestRecordToCSVLine(t *testing.T) {
	rec := Record{
		TimestampMs: 1713657600000,
		RequestID:   "req-abc",
		SourceIP:    "10.0.0.1",
		HTTPMethod:  "GET",
		APIMethod:   "GetObject",
		Bucket:      "mybucket",
		AccessKey:   "AKIAIOSFODNN7EXAMPLE",
		StatusCode:  200,
		DurationMs:  42,
	}
	line := rec.ToCSVLine()
	fields := strings.Split(line, "\u0001")
	if len(fields) != 9 {
		t.Fatalf("expected 9 SOH-delimited fields, got %d: %q", len(fields), fields)
	}
	wantFields := []string{
		"1713657600000", "req-abc", "10.0.0.1", "GET", "GetObject",
		"mybucket", "AKIAIOSFODNN7EXAMPLE", "200", "42",
	}
	for i, w := range wantFields {
		if fields[i] != w {
			t.Errorf("field[%d] = %q, want %q", i, fields[i], w)
		}
	}
}

func TestMiddlewarePopulatesAccessKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKMIDDLEWARE/20240421/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host, Signature=deadbeef")

	got := ExtractAccessKey(r)
	if got != "AKMIDDLEWARE" {
		t.Fatalf("expected AKMIDDLEWARE, got %q", got)
	}
}
