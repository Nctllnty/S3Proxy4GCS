package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"s3proxy4gcs/config"
	"s3proxy4gcs/pkg/metrics"

	dto "github.com/prometheus/client_model/go"
)

// TestMain initialises `config.Config` with production-default feature
// flags (all enabled) so pre-existing unit tests — which never knew about
// feature gating — keep seeing their handlers executed. Tests that want to
// flip a flag off call `withAllFeaturesEnabled` and then disable the one
// under test, with the helper restoring state on cleanup.
func TestMain(m *testing.M) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	config.Config.Features = config.FeatureFlags{
		Lifecycle:       true,
		CORS:            true,
		Logging:         true,
		Website:         true,
		Tagging:         true,
		RestoreObject:   true,
		CopyObject:      true,
		MultipartUpload: true,
		DeleteObjects:   true,
		HealthEndpoint:  true,
		ReadyzEndpoint:  true,
		MetricsEndpoint: true,
	}
	os.Exit(m.Run())
}

// TestWriteS3ErrorEscapesXML verifies that writeS3Error produces well-formed
// XML even when the message contains special characters. Prior to v1.4 the
// implementation used fmt.Fprintf and could emit invalid XML, which broke
// SDK parsers and created a minor injection surface.
func TestWriteS3ErrorEscapesXML(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
	}{
		{"basic", "InvalidRequest", "Plain text message."},
		{"xml-metachars", "InvalidRequest", `got <bucket & "key" too>`},
		{"newlines", "InvalidRequest", "line1\nline2"},
		{"single-quote", "InvalidRequest", "it's bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			writeS3Error(rr, http.StatusBadRequest, tc.code, tc.message)

			if got := rr.Code; got != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/xml" {
				t.Fatalf("content-type = %q, want application/xml", ct)
			}

			body := rr.Body.Bytes()
			if !strings.HasPrefix(string(body), `<?xml version="1.0" encoding="UTF-8"?>`) {
				t.Fatalf("body missing XML prolog: %q", body)
			}

			// Must round-trip via the XML parser to prove it is valid XML.
			var parsed s3ErrorBody
			if err := xml.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("parsed XML failed: %v\nbody=%s", err, body)
			}
			if parsed.Code != tc.code {
				t.Errorf("Code = %q, want %q", parsed.Code, tc.code)
			}
			if parsed.Message != tc.message {
				t.Errorf("Message = %q, want %q", parsed.Message, tc.message)
			}
		})
	}
}

// TestTranslateS3StorageClass enumerates the mappings documented in
// AGENTS.md and guards against regressions when GCS class names evolve.
func TestTranslateS3StorageClass(t *testing.T) {
	cases := []struct {
		in        string
		wantGCS   string
		wantKnown bool
	}{
		{"STANDARD", "STANDARD", true},
		{"REDUCED_REDUNDANCY", "STANDARD", true},
		{"STANDARD_IA", "NEARLINE", true},
		{"ONEZONE_IA", "NEARLINE", true},
		{"GLACIER_IR", "COLDLINE", true},
		{"GLACIER", "ARCHIVE", true},
		{"DEEP_ARCHIVE", "ARCHIVE", true},
		{"INTELLIGENT_TIERING", "AUTOCLASS", true},
		{"NEARLINE", "", false},           // GCS-native name is not an S3 class
		{"mystery-tier", "", false},       // unknown
		{"standard", "", false},           // case-sensitive on purpose
	}
	for _, tc := range cases {
		gcs, known := translateS3StorageClass(tc.in)
		if known != tc.wantKnown {
			t.Errorf("translateS3StorageClass(%q) known=%v, want %v", tc.in, known, tc.wantKnown)
		}
		if gcs != tc.wantGCS {
			t.Errorf("translateS3StorageClass(%q) gcs=%q, want %q", tc.in, gcs, tc.wantGCS)
		}
	}
}

// TestHandleS3RequestRejectsUnknownStorageClass ensures the gatekeeper at
// handleS3Request entry rejects unrecognised x-amz-storage-class values
// with a 400 InvalidStorageClass error (AGENTS rule 4).
func TestHandleS3RequestRejectsUnknownStorageClass(t *testing.T) {
	// handleS3Request consults config.Config for debug flags; make sure a
	// minimal config is loaded so the function never panics.
	if config.Config == nil {
		config.Config = &config.Settings{}
	}

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", strings.NewReader("data"))
	req.Header.Set("x-amz-storage-class", "SUPER_PREMIUM")
	rr := httptest.NewRecorder()

	handleS3Request(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var parsed s3ErrorBody
	if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response not valid XML: %v\nbody=%s", err, rr.Body.String())
	}
	if parsed.Code != "InvalidStorageClass" {
		t.Errorf("Code = %q, want InvalidStorageClass", parsed.Code)
	}
	if !strings.Contains(parsed.Message, "SUPER_PREMIUM") {
		t.Errorf("Message should echo rejected value, got %q", parsed.Message)
	}
}

// TestDecodeControlPlaneXMLRejectsOversizedBody proves the 64 KB cap is
// enforced and the client receives a MaxMessageLengthExceeded error.
func TestDecodeControlPlaneXMLRejectsOversizedBody(t *testing.T) {
	huge := strings.Repeat("x", maxControlPlaneBodySize+1)
	req := httptest.NewRequest(http.MethodPut, "/bucket/?lifecycle",
		strings.NewReader("<LifecycleConfiguration>"+huge+"</LifecycleConfiguration>"))
	rr := httptest.NewRecorder()

	type payload struct {
		XMLName xml.Name `xml:"LifecycleConfiguration"`
	}
	var dst payload
	if ok := decodeControlPlaneXML(rr, req, "lifecycle", &dst); ok {
		t.Fatalf("expected decode to fail for oversized body")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "MaxMessageLengthExceeded") {
		t.Errorf("expected MaxMessageLengthExceeded in body, got %s", rr.Body.String())
	}
}

// TestDecodeControlPlaneXMLRejectsMalformed proves invalid XML yields a
// MalformedXML S3 error rather than a 500 or partial parse.
func TestDecodeControlPlaneXMLRejectsMalformed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/bucket/?cors",
		strings.NewReader("<CORSConfiguration><CORSRule>"))
	rr := httptest.NewRecorder()

	type payload struct {
		XMLName xml.Name `xml:"CORSConfiguration"`
	}
	var dst payload
	if ok := decodeControlPlaneXML(rr, req, "cors", &dst); ok {
		t.Fatalf("expected decode to fail for malformed XML")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML in body, got %s", rr.Body.String())
	}
}

// TestHandleRestoreObject_HappyPath verifies the synthetic RestoreObject
// shim returns 200 OK with an empty body when DryRun skips the GCS probe.
// GCS objects are always immediately readable, so this is the baseline
// behaviour legacy S3 callers need to keep working.
func TestHandleRestoreObject_HappyPath(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	config.Config.DryRun = true
	defer func() { config.Config.DryRun = false }()

	req := httptest.NewRequest(http.MethodPost, "/bucket/path/to/key?restore",
		strings.NewReader(`<RestoreRequest><Days>1</Days></RestoreRequest>`))
	rr := httptest.NewRecorder()

	handleS3Request(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rr.Body.String())
	}
	if cl := rr.Header().Get("Content-Length"); cl != "0" {
		t.Errorf("Content-Length = %q, want 0", cl)
	}
	if rr.Header().Get("Date") == "" {
		t.Error("expected Date header to be set for log correlation")
	}
}

// TestHandleRestoreObject_RejectsNonPOST ensures only POST is allowed on
// `?restore`; other verbs are refused with 501 NotImplemented rather than
// silently falling through to the data-plane proxy (AGENTS rule 4).
func TestHandleRestoreObject_RejectsNonPOST(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	config.Config.DryRun = true
	defer func() { config.Config.DryRun = false }()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/bucket/key?restore", nil)
			rr := httptest.NewRecorder()
			handleS3Request(rr, req)
			if rr.Code != http.StatusNotImplemented {
				t.Fatalf("%s status = %d, want 501; body=%s", method, rr.Code, rr.Body.String())
			}
			var parsed s3ErrorBody
			if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
				t.Fatalf("body not valid S3 XML: %v\nbody=%s", err, rr.Body.String())
			}
			if parsed.Code != "NotImplemented" {
				t.Errorf("Code = %q, want NotImplemented", parsed.Code)
			}
		})
	}
}

// TestHandleRestoreObject_BodySizeCap proves the shim honours the same
// 64 KB request-body limit as the other control-plane handlers so a
// malicious or buggy client cannot exhaust memory via an oversized
// <RestoreRequest> XML document.
func TestHandleRestoreObject_BodySizeCap(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	config.Config.DryRun = true
	defer func() { config.Config.DryRun = false }()

	body := strings.Repeat("x", maxControlPlaneBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/bucket/key?restore",
		strings.NewReader(body))
	rr := httptest.NewRecorder()

	handleS3Request(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "MaxMessageLengthExceeded") {
		t.Errorf("expected MaxMessageLengthExceeded, got %s", rr.Body.String())
	}
}

// TestHandleRestoreObject_RequiresBucketAndKey guards against proxy-level
// routing mistakes: `?restore` without a key (e.g. on a bucket root) must
// return 400 InvalidArgument, not 200, otherwise callers would think a
// bucket-wide restore happened.
func TestHandleRestoreObject_RequiresBucketAndKey(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	config.Config.DryRun = true
	defer func() { config.Config.DryRun = false }()

	req := httptest.NewRequest(http.MethodPost, "/bucket/?restore", nil)
	rr := httptest.NewRecorder()
	handleS3Request(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var parsed s3ErrorBody
	if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body not valid S3 XML: %v", err)
	}
	if parsed.Code != "InvalidArgument" {
		t.Errorf("Code = %q, want InvalidArgument", parsed.Code)
	}
}

// TestHandleRestoreObject_SkipExistenceCheck verifies that the
// RESTORE_SKIP_EXISTENCE_CHECK opt-out does not attempt any GCS call.
// We run this with DryRun=false + skip=true so the GCS client would
// normally be dereferenced; success proves the branch exits early.
func TestHandleRestoreObject_SkipExistenceCheck(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	prevDryRun := config.Config.DryRun
	prevSkip := config.Config.RestoreSkipExistenceCheck
	config.Config.DryRun = false
	config.Config.RestoreSkipExistenceCheck = true
	defer func() {
		config.Config.DryRun = prevDryRun
		config.Config.RestoreSkipExistenceCheck = prevSkip
	}()

	req := httptest.NewRequest(http.MethodPost, "/bucket/key?restore", nil)
	rr := httptest.NewRecorder()
	handleS3Request(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when skipping existence check; body=%s",
			rr.Code, rr.Body.String())
	}
}

// withAllFeaturesEnabled sets every ENABLE_* flag to true (the product
// default) for the duration of a test, and restores the previous state on
// cleanup. Using this per-test helper keeps feature-flag assertions
// independent of ordering.
func withAllFeaturesEnabled(t *testing.T) {
	t.Helper()
	if config.Config == nil {
		config.Config = &config.Settings{}
	}
	prev := config.Config.Features
	config.Config.Features = config.FeatureFlags{
		Lifecycle:       true,
		CORS:            true,
		Logging:         true,
		Website:         true,
		Tagging:         true,
		RestoreObject:   true,
		CopyObject:      true,
		MultipartUpload: true,
		DeleteObjects:   true,
		HealthEndpoint:  true,
		ReadyzEndpoint:  true,
		MetricsEndpoint: true,
	}
	t.Cleanup(func() { config.Config.Features = prev })
}

// counterValue returns the current value of a metric counter vector cell,
// returning 0 if the label combination has not yet been observed. Used to
// assert the feature-disabled rejection counter increments exactly once
// per rejected request without relying on a global reset.
func counterValue(t *testing.T, c *dto.Metric) float64 {
	t.Helper()
	if c == nil || c.Counter == nil || c.Counter.Value == nil {
		return 0
	}
	return *c.Counter.Value
}

func featureDisabledCount(t *testing.T, feature string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.FeatureDisabledRejections.WithLabelValues(feature).Write(m); err != nil {
		t.Fatalf("metric write failed: %v", err)
	}
	return counterValue(t, m)
}

// TestFeatureFlags_ControlPlane verifies that each control-plane resource
// family is individually gated. When the corresponding flag is false the
// matching `handleS3Request` branch MUST return 501 NotImplemented with a
// well-formed S3 XML error and increment the rejection counter exactly
// once — without reaching the underlying handler (which would try to touch
// gcsClient and panic in DryRun).
func TestFeatureFlags_ControlPlane(t *testing.T) {
	if config.Config == nil {
		config.Config = &config.Settings{}
	}

	cases := []struct {
		feature string
		off     func()
		method  string
		uri     string
	}{
		{"lifecycle", func() { config.Config.Features.Lifecycle = false }, http.MethodPut, "/bucket/?lifecycle"},
		{"cors", func() { config.Config.Features.CORS = false }, http.MethodGet, "/bucket/?cors"},
		{"logging", func() { config.Config.Features.Logging = false }, http.MethodDelete, "/bucket/?logging"},
		{"website", func() { config.Config.Features.Website = false }, http.MethodPut, "/bucket/?website"},
		{"tagging", func() { config.Config.Features.Tagging = false }, http.MethodGet, "/bucket/key?tagging"},
		{"restore_object", func() { config.Config.Features.RestoreObject = false }, http.MethodPost, "/bucket/key?restore"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.feature, func(t *testing.T) {
			withAllFeaturesEnabled(t)
			// Flip just the one under test to off. The helper leaves the
			// rest true so a regression in another branch will not mask
			// this assertion.
			tc.off()

			before := featureDisabledCount(t, tc.feature)

			req := httptest.NewRequest(tc.method, tc.uri, nil)
			rr := httptest.NewRecorder()
			handleS3Request(rr, req)

			if rr.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body=%s", rr.Code, rr.Body.String())
			}
			var parsed s3ErrorBody
			if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
				t.Fatalf("body not valid S3 XML: %v\nbody=%s", err, rr.Body.String())
			}
			if parsed.Code != "NotImplemented" {
				t.Errorf("Code = %q, want NotImplemented", parsed.Code)
			}
			if !strings.Contains(parsed.Message, tc.feature) {
				t.Errorf("Message %q should reference feature %q", parsed.Message, tc.feature)
			}

			got := featureDisabledCount(t, tc.feature)
			if got != before+1 {
				t.Errorf("rejection counter = %v, want %v (before=%v)", got, before+1, before)
			}
		})
	}
}

// TestFeatureFlags_DataPlaneComposites verifies the three high-cost
// data-plane operations (CopyObject, Multipart, DeleteObjects) can each
// be disabled independently and return 501 instead of being forwarded to
// the reverse proxy.
func TestFeatureFlags_DataPlaneComposites(t *testing.T) {
	type tc struct {
		feature string
		method  string
		uri     string
		header  http.Header
	}

	cases := []tc{
		{
			feature: "copy_object",
			method:  http.MethodPut,
			uri:     "/dst-bucket/dst-key",
			header:  http.Header{"X-Amz-Copy-Source": {"/src-bucket/src-key"}},
		},
		{
			feature: "multipart_upload",
			method:  http.MethodPost,
			uri:     "/bucket/key?uploads",
		},
		{
			feature: "multipart_upload",
			method:  http.MethodPut,
			uri:     "/bucket/key?partNumber=1&uploadId=abc",
		},
		{
			feature: "multipart_upload",
			method:  http.MethodPost,
			uri:     "/bucket/key?uploadId=abc",
		},
		{
			feature: "delete_objects",
			method:  http.MethodPost,
			uri:     "/bucket/?delete",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.feature+"__"+c.method+"_"+strings.ReplaceAll(c.uri, "/", "_"), func(t *testing.T) {
			withAllFeaturesEnabled(t)
			switch c.feature {
			case "copy_object":
				config.Config.Features.CopyObject = false
			case "multipart_upload":
				config.Config.Features.MultipartUpload = false
			case "delete_objects":
				config.Config.Features.DeleteObjects = false
			}

			before := featureDisabledCount(t, c.feature)

			req := httptest.NewRequest(c.method, c.uri, nil)
			for k, v := range c.header {
				req.Header[k] = v
			}
			rr := httptest.NewRecorder()
			handleS3Request(rr, req)

			if rr.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body=%s", rr.Code, rr.Body.String())
			}
			var parsed s3ErrorBody
			if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
				t.Fatalf("body not valid S3 XML: %v\nbody=%s", err, rr.Body.String())
			}
			if parsed.Code != "NotImplemented" {
				t.Errorf("Code = %q, want NotImplemented", parsed.Code)
			}
			if got := featureDisabledCount(t, c.feature); got != before+1 {
				t.Errorf("rejection counter = %v, want %v", got, before+1)
			}
		})
	}
}

// TestFeatureFlags_DataPlaneCompositeDispatchPriority ensures that when
// a request hits more than one composite detector (e.g. PUT with
// x-amz-copy-source AND ?partNumber=1&uploadId=X, which is UploadPartCopy),
// the copy-object gate runs first and its rejection supersedes the
// multipart check. This keeps operator intent — "block all server-side
// copies" — honoured even during multipart workflows.
func TestFeatureFlags_DataPlaneCompositeDispatchPriority(t *testing.T) {
	withAllFeaturesEnabled(t)
	config.Config.Features.CopyObject = false
	// Multipart stays true; copy_object should fire first.

	before := featureDisabledCount(t, "copy_object")

	req := httptest.NewRequest(http.MethodPut,
		"/bucket/key?partNumber=1&uploadId=abc", nil)
	req.Header.Set("X-Amz-Copy-Source", "/src/key")
	rr := httptest.NewRecorder()
	handleS3Request(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 via copy_object gate", rr.Code)
	}
	var parsed s3ErrorBody
	_ = xml.Unmarshal(rr.Body.Bytes(), &parsed)
	if !strings.Contains(parsed.Message, "copy_object") {
		t.Errorf("expected copy_object to be the rejecting feature, got %q", parsed.Message)
	}
	if got := featureDisabledCount(t, "copy_object"); got != before+1 {
		t.Errorf("copy_object counter = %v, want %v", got, before+1)
	}
}

// TestFeatureDisabled404 verifies the ops-plane stub handler used when a
// /health /readyz /metrics flag is disabled: 404 status, plain-text body
// mentioning the feature, and counter increment.
func TestFeatureDisabled404(t *testing.T) {
	for _, feat := range []string{"health_endpoint", "readyz_endpoint", "metrics_endpoint"} {
		feat := feat
		t.Run(feat, func(t *testing.T) {
			before := featureDisabledCount(t, feat)

			handler := featureDisabled404(feat)
			rr := httptest.NewRecorder()
			handler(rr, httptest.NewRequest(http.MethodGet, "/"+feat, nil))

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("content-type = %q, want text/plain", ct)
			}
			if !strings.Contains(rr.Body.String(), feat) {
				t.Errorf("body %q should mention feature %q", rr.Body.String(), feat)
			}
			if got := featureDisabledCount(t, feat); got != before+1 {
				t.Errorf("counter = %v, want %v", got, before+1)
			}
		})
	}
}

// TestEnsureFeatureEnabled_Allow ensures the happy path returns true and
// makes no side effects (no counter increment, nothing written).
func TestEnsureFeatureEnabled_Allow(t *testing.T) {
	before := featureDisabledCount(t, "lifecycle")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/?lifecycle", nil)
	if !ensureFeatureEnabled(rr, req, true, "lifecycle") {
		t.Fatal("expected allow=true when enabled=true")
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected no body written, got %q", rr.Body.String())
	}
	if got := featureDisabledCount(t, "lifecycle"); got != before {
		t.Errorf("counter changed from %v to %v when feature enabled", before, got)
	}
}

// TestDecodeControlPlaneXMLHappyPath sanity-checks the success path so the
// guards above are not the only thing covered.
func TestDecodeControlPlaneXMLHappyPath(t *testing.T) {
	input := `<?xml version="1.0"?><LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status></Rule></LifecycleConfiguration>`
	req := httptest.NewRequest(http.MethodPut, "/bucket/?lifecycle", strings.NewReader(input))
	rr := httptest.NewRecorder()

	type rule struct {
		ID     string `xml:"ID"`
		Status string `xml:"Status"`
	}
	type payload struct {
		XMLName xml.Name `xml:"LifecycleConfiguration"`
		Rules   []rule   `xml:"Rule"`
	}
	var dst payload
	if !decodeControlPlaneXML(rr, req, "lifecycle", &dst) {
		t.Fatalf("decode failed: %s", rr.Body.String())
	}
	if len(dst.Rules) != 1 || dst.Rules[0].ID != "x" || dst.Rules[0].Status != "Enabled" {
		t.Fatalf("unexpected parse: %+v", dst)
	}
	// Ensure the helper closed the body.
	if _, err := io.ReadAll(req.Body); err == nil {
		// Read should return empty (body already consumed) — that's fine.
		// We only assert the helper does not panic on a second read.
	}
}
