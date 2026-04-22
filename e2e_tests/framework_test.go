package e2e

import (
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

// testEnv is the shared environment configuration for all tests.
var testEnv *Env

func TestMain(m *testing.M) {
	directMode := os.Getenv("BENCH_MODE") == "direct"

	// 1. Load and validate environment
	env, err := LoadEnv()
	if err != nil {
		if directMode {
			// In direct mode, HMAC credentials are required for SDK parity
			// with proxy benchmarks. PROXY_ENDPOINT is not needed.
			env = &Env{
				ProxyEndpoint: "https://storage.googleapis.com",
				HMACAccess:    os.Getenv("GCS_HMAC_ACCESS"),
				HMACSecret:    os.Getenv("GCS_HMAC_SECRET"),
				TestBucket:    os.Getenv("TEST_BUCKET"),
				TestPrefix:    os.Getenv("TEST_PREFIX"),
			}
			if env.TestBucket == "" {
				log.Fatalf("Direct mode requires TEST_BUCKET")
			}
			if env.HMACAccess == "" || env.HMACSecret == "" {
				log.Fatalf("Direct mode requires GCS_HMAC_ACCESS and GCS_HMAC_SECRET for SDK parity")
			}
		} else {
			log.Fatalf("E2E Environment setup failed: %v", err)
		}
	}
	testEnv = env

	if directMode {
		fmt.Println("========================================")
		fmt.Println("  Direct GCS S3-Compatible API Benchmark")
		fmt.Println("========================================")
		fmt.Printf("  GCS Endpoint   : https://storage.googleapis.com\n")
		fmt.Printf("  Test Bucket    : %s\n", env.TestBucket)
		fmt.Printf("  Test Prefix    : %s\n", env.TestPrefix)
		fmt.Println("========================================")
	} else {
		fmt.Println("========================================")
		fmt.Println("  S3Proxy4GCS E2E Acceptance Tests")
		fmt.Println("========================================")
		fmt.Printf("  Proxy Endpoint : %s\n", env.ProxyEndpoint)
		fmt.Printf("  Test Bucket    : %s\n", env.TestBucket)
		fmt.Printf("  Test Prefix    : %s\n", env.TestPrefix)
		fmt.Println("========================================")

		// 2. Wait for proxy to be healthy (skip in direct mode)
		fmt.Println("Waiting for proxy to become healthy...")
		if err := WaitForProxy(env.ProxyEndpoint, 30*time.Second); err != nil {
			log.Fatalf("Proxy health check failed: %v", err)
		}
		fmt.Println("Proxy is healthy. Starting tests...")
	}
	fmt.Println()

	// 3. Run all tests
	code := m.Run()

	// 4. Summary
	fmt.Println()
	fmt.Println("========================================")
	if code == 0 {
		fmt.Println("  ALL E2E TESTS PASSED")
	} else {
		fmt.Println("  SOME E2E TESTS FAILED")
	}
	fmt.Println("========================================")

	os.Exit(code)
}
