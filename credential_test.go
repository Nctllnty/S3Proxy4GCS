package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"s3proxy4gcs/pkg/credstore"
)

// withHMACStore swaps the package-global hmacCredentials for the duration
// of a subtest and restores it afterwards. Keeping the swap local per-test
// lets us run the suite with -parallel=1 without leaking state into
// unrelated assertions in main_test.go that assume the store is empty.
func withHMACStore(t *testing.T, seed map[string]string) {
	t.Helper()
	prev := hmacCredentials
	replacement := credstore.New()
	if len(seed) > 0 {
		replacement.Replace(seed)
	}
	hmacCredentials = replacement
	t.Cleanup(func() { hmacCredentials = prev })
}

func decodeS3Error(t *testing.T, body io.Reader) s3ErrorBody {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	// writeS3Error emits an XML prolog followed by the body; strip the
	// prolog so xml.Unmarshal sees just the element.
	trimmed := strings.TrimSpace(string(raw))
	if i := strings.Index(trimmed, "?>"); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[i+2:])
	}
	var out s3ErrorBody
	if err := xml.Unmarshal([]byte(trimmed), &out); err != nil {
		t.Fatalf("unmarshal body %q: %v", trimmed, err)
	}
	return out
}

// TestValidateClientCredentialDisabled asserts the fast-path when no
// credential mapping is configured: the function returns (_, false) with
// no response written so the caller can fall back to the legacy single
// key path without any side-effects.
func TestValidateClientCredentialDisabled(t *testing.T) {
	withHMACStore(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	creds, ok := validateClientCredential(rec, req)
	if ok {
		t.Fatal("expected ok=false when store is empty")
	}
	if creds.AccessKeyID != "" || creds.SecretAccessKey != "" {
		t.Errorf("creds should be zero when store is empty, got %+v", creds)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("no response should be written; got status %d", rec.Code)
	}
}

// TestValidateClientCredentialHit is the happy path: a request with a
// valid SigV4 Authorization header whose AK is in the map resolves to
// the matching SK and stashes the result in the request context so the
// Director can pick it up later.
func TestValidateClientCredentialHit(t *testing.T) {
	withHMACStore(t, map[string]string{"GOOG1EAK": "SK-VALUE"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=GOOG1EAK/20260101/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=deadbeef")

	creds, ok := validateClientCredential(rec, req)
	if !ok {
		t.Fatalf("expected ok=true, got response: %s", rec.Body.String())
	}
	if creds.AccessKeyID != "GOOG1EAK" || creds.SecretAccessKey != "SK-VALUE" {
		t.Errorf("creds = %+v, want GOOG1EAK/SK-VALUE", creds)
	}
	// The function MUST have propagated the resolved credentials onto
	// the request context so the Director picks them up.
	ctxCreds, hasCtx := credentialsFromContext(req.Context())
	if !hasCtx {
		t.Fatal("resolved credentials missing from request context")
	}
	if ctxCreds.AccessKeyID != "GOOG1EAK" || ctxCreds.SecretAccessKey != "SK-VALUE" {
		t.Errorf("ctx creds = %+v, want GOOG1EAK/SK-VALUE", ctxCreds)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("no error should be written; got status %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestValidateClientCredentialMiss asserts that an AK not in the map
// yields exactly the S3 error the AWS SDKs know how to surface
// (`InvalidAccessKeyId` with 403), so migration operators see the same
// failure mode they would from real S3 / GCS.
func TestValidateClientCredentialMiss(t *testing.T) {
	withHMACStore(t, map[string]string{"KNOWN": "SECRET"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=UNKNOWN/20260101/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host, Signature=abc")

	_, ok := validateClientCredential(rec, req)
	if ok {
		t.Fatal("expected ok=false for unknown AK")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	body := decodeS3Error(t, rec.Body)
	if body.Code != "InvalidAccessKeyId" {
		t.Errorf("code = %q, want InvalidAccessKeyId", body.Code)
	}
}

// TestValidateClientCredentialNoAuth covers anonymous requests against a
// gated proxy: missing Authorization + missing X-Amz-Credential must
// short-circuit with AccessDenied.
func TestValidateClientCredentialNoAuth(t *testing.T) {
	withHMACStore(t, map[string]string{"AK": "SK"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	_, ok := validateClientCredential(rec, req)
	if ok {
		t.Fatal("expected ok=false when no Authorization header")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	body := decodeS3Error(t, rec.Body)
	if body.Code != "AccessDenied" {
		t.Errorf("code = %q, want AccessDenied", body.Code)
	}
}

// TestValidateClientCredentialPresignedQuery exercises the presigned-URL
// path: the AK lives in `X-Amz-Credential` instead of the Authorization
// header. GCS HMAC presigned URLs use this shape for short-lived
// download/upload links, and we must not reject them as anonymous.
func TestValidateClientCredentialPresignedQuery(t *testing.T) {
	withHMACStore(t, map[string]string{"GOOG1EAK": "SK-VALUE"})

	rec := httptest.NewRecorder()
	rawURL := "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=GOOG1EAK%2F20260101%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20260101T000000Z" +
		"&X-Amz-Expires=900" +
		"&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=deadbeef"
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)

	creds, ok := validateClientCredential(rec, req)
	if !ok {
		t.Fatalf("expected ok=true, got response: %s", rec.Body.String())
	}
	if creds.AccessKeyID != "GOOG1EAK" {
		t.Errorf("AK = %q, want GOOG1EAK", creds.AccessKeyID)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("no error should be written; got %d", rec.Code)
	}
}

// TestCredentialsFromContextEmpty guards the Director fallback: when the
// handler has not injected credentials (legacy DryRun path), the helper
// must return (_, false) and NOT panic on a nil value.
func TestCredentialsFromContextEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	_, ok := credentialsFromContext(req.Context())
	if ok {
		t.Fatal("expected ok=false on context without resolved creds")
	}
}
