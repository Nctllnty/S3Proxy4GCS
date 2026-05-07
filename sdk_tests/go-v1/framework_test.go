// Package gov1 provides E2E functional tests using AWS SDK for Go V1 (v1.50.0).
package gov1

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type Env struct {
	ProxyEndpoint string
	HMACAccess    string
	HMACSecret    string
	TestBucket    string
	TestPrefix    string
}

// sdkTestBucket is the default GCS bucket for Go V1 SDK compatibility tests.
// Override via TEST_BUCKET env var for different environments (e.g. production).
const sdkTestBucket = "s3proxy-sdk-go-v1"

var testEnv *Env

func TestMain(m *testing.M) {
	env, err := LoadEnv()
	if err != nil {
		log.Fatalf("Environment setup failed: %v", err)
	}
	testEnv = env

	fmt.Println("========================================")
	fmt.Println("  Go V1 SDK (v1.50.0) Functional Tests")
	fmt.Println("========================================")
	fmt.Printf("  Proxy Endpoint : %s\n", env.ProxyEndpoint)
	fmt.Printf("  Test Bucket    : %s\n", env.TestBucket)
	fmt.Println("========================================")

	fmt.Println("Waiting for proxy to become healthy...")
	if err := WaitForProxy(env.ProxyEndpoint, 60*time.Second); err != nil {
		log.Fatalf("Proxy health check failed: %v", err)
	}
	fmt.Println("Proxy is healthy. Starting tests...")

	code := m.Run()
	if code == 0 {
		fmt.Println("ALL GO V1 TESTS PASSED")
	} else {
		fmt.Println("SOME GO V1 TESTS FAILED")
	}
	os.Exit(code)
}

func getTestBucket() string {
	if b := os.Getenv("TEST_BUCKET"); b != "" {
		return b
	}
	return sdkTestBucket
}

func LoadEnv() (*Env, error) {
	e := &Env{
		ProxyEndpoint: os.Getenv("PROXY_ENDPOINT"),
		HMACAccess:    os.Getenv("GCS_HMAC_ACCESS"),
		HMACSecret:    os.Getenv("GCS_HMAC_SECRET"),
		TestBucket:    getTestBucket(),
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

func NewS3Client(t *testing.T, env *Env) *s3.S3 {
	t.Helper()
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(env.ProxyEndpoint),
		Region:           aws.String("us-east-1"),
		Credentials:      credentials.NewStaticCredentials(env.HMACAccess, env.HMACSecret, ""),
		S3ForcePathStyle: aws.Bool(true),
		// Disable MD5 validation and the 100-continue handshake so the
		// SDK sends a plain sha256 payload that GCS XML API can verify
		// after proxy re-signing. Without these, Go V1 may add Expect:
		// 100-continue and Content-MD5 headers that either confuse GCS
		// or mismatch after re-signing.
		S3DisableContentMD5Validation: aws.Bool(true),
		S3Disable100Continue:          aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	return s3.New(sess)
}

func GenerateTestKey(env *Env, suffix string) string {
	return fmt.Sprintf("%s%s-%d", env.TestPrefix, suffix, time.Now().UnixNano())
}

func Cleanup(t *testing.T, client *s3.S3, bucket string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		_, err := client.DeleteObject(&s3.DeleteObjectInput{
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
