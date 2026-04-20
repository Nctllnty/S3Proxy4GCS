package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestRestoreObjectWithAWSSDK exercises the synthetic RestoreObject handler
// end-to-end. GCS has no notion of "thawing", so the proxy must synthesise
// an S3-compatible response for `POST /<bucket>/<key>?restore` even though
// GCS itself would reply 400 InvalidArgument for the same call.
//
// Covered cases:
//   - Happy path on an existing key via the AWS Go V2 SDK → no error.
//   - Missing key via raw signed HTTP → 404 NoSuchKey XML body. We avoid
//     driving the missing-key case through the SDK because the Go V2
//     RestoreObject operation has historically swallowed unexpected 4xx
//     responses without surfacing them as an error; testing the wire-level
//     response keeps the assertion focused on proxy behaviour rather than
//     SDK quirks.
func TestRestoreObjectWithAWSSDK(t *testing.T) {
	transport := newProxyLoopbackTransport()

	creds := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     getAWSAccessKey(),
			SecretAccessKey: getAWSSecretKey(),
			Source:          "test-env",
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(creds),
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: transport}
		o.BaseEndpoint = aws.String("http://storage.googleapis.com")
	})

	bucketName := getTestBucket()
	prefix := getTestPrefix()

	t.Run("HappyPath_LiveObject", func(t *testing.T) {
		objectKey := prefix + "restore-object-live"

		if _, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objectKey),
			Body:   strings.NewReader("restore-me"),
		}); err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}

		if _, err := client.RestoreObject(context.TODO(), &s3.RestoreObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objectKey),
			RestoreRequest: &types.RestoreRequest{
				Days: aws.Int32(1),
				GlacierJobParameters: &types.GlacierJobParameters{
					Tier: types.TierStandard,
				},
			},
		}); err != nil {
			t.Fatalf("RestoreObject failed on an existing GCS object: %v", err)
		}
		t.Logf("RestoreObject via AWS SDK succeeded on existing key")
	})

	t.Run("MissingKey_Returns404", func(t *testing.T) {
		missingKey := prefix + "restore-object-missing-" + randomSuffix()

		status, body, err := sendSignedRestoreRequest(t, transport,
			bucketName, missingKey,
			getAWSAccessKey(), getAWSSecretKey())
		if err != nil {
			t.Fatalf("signed request failed to reach proxy: %v", err)
		}

		if status != http.StatusNotFound {
			t.Fatalf("expected 404 for missing key, got %d; body=%s", status, body)
		}
		if !strings.Contains(body, "<Code>NoSuchKey</Code>") {
			t.Fatalf("expected NoSuchKey S3 error, got body=%s", body)
		}
		t.Logf("Proxy correctly synthesised NoSuchKey for missing object")
	})

	t.Run("RejectsNonPOST_WithNotImplemented", func(t *testing.T) {
		// Send GET /<bucket>/<key>?restore — anonymous request is fine here
		// because the proxy refuses non-POST ?restore before any signing
		// verification would happen.
		req, _ := http.NewRequest(http.MethodGet,
			"http://storage.googleapis.com/"+getTestBucket()+"/someKey?restore", nil)
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501, got %d; body=%s", resp.StatusCode, string(b))
		}
	})
}

// newProxyLoopbackTransport returns an http.Transport that rewrites
// storage.googleapis.com to the in-process proxy listening on 8081, so
// AWS SDK clients can target a stable hostname while reaching our test
// binary.
func newProxyLoopbackTransport() *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "storage.googleapis.com") {
				return dialer.DialContext(ctx, network, "localhost:8081")
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
}

// sendSignedRestoreRequest builds a `POST /<bucket>/<key>?restore` request,
// SigV4-signs it with the proxy-facing credentials, dispatches it through
// the loopback transport and returns (statusCode, body, err). Signing is
// required because the proxy rejects unauthenticated data-plane calls.
func sendSignedRestoreRequest(t *testing.T, transport http.RoundTripper,
	bucket, key, accessKey, secretKey string) (int, string, error) {
	t.Helper()

	urlStr := "http://storage.googleapis.com/" + bucket + "/" + key + "?restore"
	req, err := http.NewRequest(http.MethodPost, urlStr, strings.NewReader(""))
	if err != nil {
		return 0, "", err
	}
	// Empty-body hash; AWS SDK would set this header, we match that.
	emptyHash := fmt.Sprintf("%x", sha256.Sum256(nil))
	req.Header.Set("x-amz-content-sha256", emptyHash)

	signer := v4.NewSigner()
	if err := signer.SignHTTP(context.TODO(),
		aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		req, emptyHash, "s3", "us-east-1", time.Now().UTC()); err != nil {
		return 0, "", fmt.Errorf("SigV4 signing failed: %w", err)
	}

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// randomSuffix returns a short pseudo-unique token so parallel test runs
// do not collide on the same missing-key probe.
func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
