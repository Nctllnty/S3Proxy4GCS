// Package gov2 provides E2E functional tests using AWS SDK for Go V2 (v1.75.1).
package gov2

import (
	"context"
	"fmt"
	"log"
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
	ProxyEndpoint string
	HMACAccess    string
	HMACSecret    string
	TestBucket    string
	TestPrefix    string
}

// sdkTestBucket is the real GCS bucket dedicated to Go V2 SDK compatibility tests.
// Provisioned once in project `cbs-poctest` (US-EAST1) and hard-coded here — no
// TEST_BUCKET env indirection so each SDK owns an isolated bucket.
const sdkTestBucket = "s3proxy-sdk-go-v2"

var testEnv *Env

func TestMain(m *testing.M) {
	env, err := LoadEnv()
	if err != nil {
		log.Fatalf("Environment setup failed: %v", err)
	}
	testEnv = env

	fmt.Println("========================================")
	fmt.Println("  Go V2 SDK (v1.75.1) Functional Tests")
	fmt.Println("========================================")
	fmt.Printf("  Proxy Endpoint : %s\n", env.ProxyEndpoint)
	fmt.Printf("  Test Bucket    : %s\n", env.TestBucket)
	fmt.Printf("  Test Prefix    : %s\n", env.TestPrefix)
	fmt.Println("========================================")

	fmt.Println("Waiting for proxy to become healthy...")
	if err := WaitForProxy(env.ProxyEndpoint, 60*time.Second); err != nil {
		log.Fatalf("Proxy health check failed: %v", err)
	}
	fmt.Println("Proxy is healthy. Starting tests...")

	code := m.Run()

	fmt.Println()
	if code == 0 {
		fmt.Println("ALL GO V2 TESTS PASSED")
	} else {
		fmt.Println("SOME GO V2 TESTS FAILED")
	}
	os.Exit(code)
}

func LoadEnv() (*Env, error) {
	e := &Env{
		ProxyEndpoint: os.Getenv("PROXY_ENDPOINT"),
		HMACAccess:    os.Getenv("GCS_HMAC_ACCESS"),
		HMACSecret:    os.Getenv("GCS_HMAC_SECRET"),
		TestBucket:    sdkTestBucket,
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if e.TestPrefix != "" && !strings.HasSuffix(e.TestPrefix, "/") {
		e.TestPrefix += "/"
	}
	return e, nil
}

func NewS3Client(t *testing.T, env *Env) *s3.Client {
	t.Helper()
	proxyURL, err := url.Parse(env.ProxyEndpoint)
	if err != nil {
		t.Fatalf("Failed to parse PROXY_ENDPOINT: %v", err)
	}
	_ = proxyURL

	creds := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     env.HMACAccess,
			SecretAccessKey: env.HMACSecret,
			Source:          "sdk-test-go-v2",
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
		o.BaseEndpoint = aws.String(env.ProxyEndpoint)
		// Force PutObject to use a non-streaming payload (real sha256,
		// no trailer checksum) so the GCS XML API can verify the
		// re-signed request. Without these flags the SDK may wrap the
		// body with STREAMING-UNSIGNED-PAYLOAD-TRAILER, which GCS does
		// not accept.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.HTTPClient = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 60 * time.Second,
		}
	})
	return client
}

func GenerateTestKey(env *Env, suffix string) string {
	return fmt.Sprintf("%s%s-%d", env.TestPrefix, suffix, time.Now().UnixNano())
}

func Cleanup(t *testing.T, client *s3.Client, bucket string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		_, err := client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Logf("[cleanup] Failed to delete %s: %v", key, err)
		}
	}
}

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
