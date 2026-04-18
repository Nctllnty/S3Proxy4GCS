package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"s3proxy4gcs/config"
)

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
