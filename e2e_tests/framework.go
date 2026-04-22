// Package e2e provides shared test infrastructure for E2E acceptance tests
// against a live S3Proxy4GCS instance.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Env holds the resolved test configuration from environment variables.
type Env struct {
	ProxyEndpoint string // e.g. http://s3proxy.default.svc:8080
	HMACAccess    string
	HMACSecret    string
	TestBucket    string
	TestPrefix    string // e.g. "e2e-12345/" for run isolation
}

// LoadEnv reads and validates required environment variables.
// It calls t.Fatal (or log.Fatal in TestMain) if any required var is missing.
func LoadEnv() (*Env, error) {
	e := &Env{
		ProxyEndpoint: os.Getenv("PROXY_ENDPOINT"),
		HMACAccess:    os.Getenv("GCS_HMAC_ACCESS"),
		HMACSecret:    os.Getenv("GCS_HMAC_SECRET"),
		TestBucket:    os.Getenv("TEST_BUCKET"),
		TestPrefix:    os.Getenv("TEST_PREFIX"),
	}

	var missing []string
	if e.ProxyEndpoint == "" {
		missing = append(missing, "PROXY_ENDPOINT")
	}
	if e.HMACAccess == "" {
		missing = append(missing, "GCS_HMAC_ACCESS")
	}
	if e.HMACSecret == "" {
		missing = append(missing, "GCS_HMAC_SECRET")
	}
	if e.TestBucket == "" {
		missing = append(missing, "TEST_BUCKET")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Ensure prefix ends with /
	if e.TestPrefix != "" && !strings.HasSuffix(e.TestPrefix, "/") {
		e.TestPrefix += "/"
	}

	return e, nil
}

// NewS3Client creates an AWS S3 client configured to route all requests through
// the proxy endpoint. It uses GCS HMAC credentials for authentication.
func NewS3Client(t *testing.T, env *Env) *s3.Client {
	t.Helper()

	proxyURL, err := url.Parse(env.ProxyEndpoint)
	if err != nil {
		t.Fatalf("Failed to parse PROXY_ENDPOINT: %v", err)
	}

	creds := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     env.HMACAccess,
			SecretAccessKey: env.HMACSecret,
			Source:          "e2e-test-env",
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(creds),
		config.WithRegion("us-east-1"),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(env.ProxyEndpoint)
		o.HTTPClient = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 60 * time.Second,
		}
		_ = proxyURL // Used for potential future transport customization
	})

	return client
}

// GenerateTestKey returns a unique object key scoped to the test prefix.
func GenerateTestKey(env *Env, suffix string) string {
	ts := time.Now().UnixNano()
	return fmt.Sprintf("%s%s-%d", env.TestPrefix, suffix, ts)
}

// Cleanup deletes the given object keys from the bucket, ignoring errors.
// Intended to be called in a defer or t.Cleanup.
func Cleanup(t *testing.T, client *s3.Client, bucket string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		_, err := client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Logf("[cleanup] Failed to delete %s/%s: %v", bucket, key, err)
		}
	}
}

// WaitForProxy polls the proxy's /health endpoint until it returns 200 or timeout.
func WaitForProxy(endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	healthURL := strings.TrimRight(endpoint, "/") + "/health"

	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("proxy at %s did not become healthy within %v", endpoint, timeout)
}
