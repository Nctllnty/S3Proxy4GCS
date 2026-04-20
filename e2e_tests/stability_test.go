package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func getStabilityRounds() int {
	if v := os.Getenv("STABILITY_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

func getConcurrency() int {
	if v := os.Getenv("CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}

func getStabilityDurationSec() int {
	if v := os.Getenv("STABILITY_DURATION_SEC"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			return n
		}
	}
	return 120 // default 2 minutes
}

// TestLongRunningCRUD repeatedly executes Object CRUD in a loop to verify stability.
// Collects system metrics before/after and reports resource impact.
func TestLongRunningCRUD(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	rounds := getStabilityRounds()
	mc := NewMetricsCollector(testEnv.ProxyEndpoint)

	t.Logf("Starting long-running CRUD stability test: %d rounds", rounds)

	before, _ := mc.Snapshot()
	var totalDuration time.Duration
	successCount := 0

	for i := 0; i < rounds; i++ {
		key := fmt.Sprintf("%sstability-crud-%d-%d", testEnv.TestPrefix, time.Now().UnixNano(), i)
		content := fmt.Sprintf("stability test round %d", i)

		start := time.Now()

		// Put
		_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   strings.NewReader(content),
		})
		if err != nil {
			t.Fatalf("Round %d: PutObject failed: %v", i, err)
		}

		// Get + verify
		getOut, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("Round %d: GetObject failed: %v", i, err)
		}
		body, _ := io.ReadAll(getOut.Body)
		getOut.Body.Close()
		if string(body) != content {
			t.Fatalf("Round %d: body mismatch: got %q, want %q", i, string(body), content)
		}

		// Delete
		_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("Round %d: DeleteObject failed: %v", i, err)
		}

		elapsed := time.Since(start)
		totalDuration += elapsed
		successCount++

		if (i+1)%10 == 0 || i == rounds-1 {
			avg := totalDuration / time.Duration(successCount)
			t.Logf("Round %d/%d complete (avg latency: %v)", i+1, rounds, avg)
		}
	}

	after, _ := mc.Snapshot()
	delta := ComputeDelta(before, after, totalDuration)

	avg := totalDuration / time.Duration(successCount)
	t.Logf("Stability CRUD test complete: %d/%d rounds passed, avg latency: %v", successCount, rounds, avg)
	t.Logf("[system] CPU=%.1f%%  MemΔ=%.1fMB  PeakMem=%.1fMB  GoroutineΔ=%.0f  HTTPΔ=%.0f",
		delta.CPUUsagePercent, delta.MemoryDeltaMB, delta.PeakResidentMB,
		delta.GoroutineDelta, delta.HTTPRequestsDelta)
}

// TestConcurrentOperations runs N goroutines each performing CRUD to verify
// the proxy handles concurrent data-plane requests without errors or data mixing.
// Supports duration-based mode via STABILITY_DURATION_SEC.
func TestConcurrentOperations(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	concurrency := getConcurrency()
	durationSec := getStabilityDurationSec()
	duration := time.Duration(durationSec) * time.Second
	mc := NewMetricsCollector(testEnv.ProxyEndpoint)

	t.Logf("Starting concurrent operations test: %d goroutines, %ds duration", concurrency, durationSec)

	before, _ := mc.Snapshot()
	start := time.Now()
	deadline := start.Add(duration)

	var wg sync.WaitGroup
	var totalOps atomic.Int64
	var failures atomic.Int64
	var dataMixing atomic.Int64

	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			iter := 0

			for time.Now().Before(deadline) {
				key := fmt.Sprintf("%sconcurrent-%d-%d-%d", testEnv.TestPrefix, goroutineID, time.Now().UnixNano(), iter)
				content := fmt.Sprintf("goroutine-%d-iter-%d", goroutineID, iter)

				// Put
				_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
					Body:   strings.NewReader(content),
				})
				if err != nil {
					failures.Add(1)
					totalOps.Add(1)
					iter++
					continue
				}

				// Get + verify (no data mixing)
				getOut, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
				})
				if err != nil {
					failures.Add(1)
					totalOps.Add(1)
					// cleanup
					client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
					iter++
					continue
				}
				body, _ := io.ReadAll(getOut.Body)
				getOut.Body.Close()
				if string(body) != content {
					dataMixing.Add(1)
					t.Errorf("Goroutine %d iter %d: DATA MIXING! got %q, want %q", goroutineID, iter, string(body), content)
				}

				// Delete
				_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
				})
				if err != nil {
					failures.Add(1)
				}

				totalOps.Add(1)
				iter++
			}
		}(g)
	}

	wg.Wait()
	wallClock := time.Since(start)

	after, _ := mc.Snapshot()
	delta := ComputeDelta(before, after, wallClock)

	ops := totalOps.Load()
	fails := failures.Load()
	mixing := dataMixing.Load()
	opsPerSec := float64(ops) / wallClock.Seconds()

	t.Logf("Concurrent test complete: %d ops in %.1fs (%.1f ops/s), %d failures, %d data-mixing",
		ops, wallClock.Seconds(), opsPerSec, fails, mixing)
	t.Logf("[system] CPU=%.1f%%  MemΔ=%.1fMB  PeakMem=%.1fMB  GoroutineΔ=%.0f  HTTPΔ=%.0f",
		delta.CPUUsagePercent, delta.MemoryDeltaMB, delta.PeakResidentMB,
		delta.GoroutineDelta, delta.HTTPRequestsDelta)

	if mixing > 0 {
		t.Fatalf("DATA MIXING DETECTED: %d instances across %d total ops", mixing, ops)
	}
	// Allow a small error rate (< 1%) for transient network issues
	errorRate := float64(fails) / float64(ops) * 100
	if errorRate > 1.0 {
		t.Fatalf("Error rate too high: %.2f%% (%d/%d)", errorRate, fails, ops)
	}
}

// TestControlPlaneConcurrency runs a small number of goroutines doing PutBucketCors + GetBucketCors
// with pacing to stay within GCS bucket-metadata rate limits (~1 write/sec/bucket).
// The goal is to verify concurrent control plane operations don't crash or corrupt each other.
func TestControlPlaneConcurrency(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	// Use fixed low concurrency (3) for control plane — GCS rate-limits bucket metadata writes.
	const cpConcurrency = 3
	durationSec := getStabilityDurationSec()
	duration := time.Duration(durationSec) * time.Second
	mc := NewMetricsCollector(testEnv.ProxyEndpoint)

	t.Cleanup(func() {
		client.DeleteBucketCors(context.TODO(), &s3.DeleteBucketCorsInput{
			Bucket: aws.String(bucket),
		})
	})

	t.Logf("Starting control plane concurrency test: %d goroutines, %ds duration, 500ms pacing", cpConcurrency, durationSec)

	before, _ := mc.Snapshot()
	start := time.Now()
	deadline := start.Add(duration)

	var wg sync.WaitGroup
	var totalOps atomic.Int64
	var failures atomic.Int64

	for g := 0; g < cpConcurrency; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for time.Now().Before(deadline) {
				// PutBucketCors
				_, err := client.PutBucketCors(context.TODO(), &s3.PutBucketCorsInput{
					Bucket: aws.String(bucket),
					CORSConfiguration: &types.CORSConfiguration{
						CORSRules: []types.CORSRule{
							{
								AllowedMethods: []string{"GET"},
								AllowedOrigins: []string{fmt.Sprintf("https://g%d.example.com", goroutineID)},
							},
						},
					},
				})
				if err != nil {
					failures.Add(1)
					totalOps.Add(1)
					time.Sleep(500 * time.Millisecond)
					continue
				}

				// GetBucketCors
				_, err = client.GetBucketCors(context.TODO(), &s3.GetBucketCorsInput{
					Bucket: aws.String(bucket),
				})
				if err != nil {
					failures.Add(1)
				}
				totalOps.Add(1)

				// Pace to respect GCS bucket-metadata rate limits
				time.Sleep(500 * time.Millisecond)
			}
		}(g)
	}

	wg.Wait()
	wallClock := time.Since(start)

	after, _ := mc.Snapshot()
	delta := ComputeDelta(before, after, wallClock)

	ops := totalOps.Load()
	fails := failures.Load()
	opsPerSec := float64(ops) / wallClock.Seconds()

	t.Logf("Control plane concurrency complete: %d ops in %.1fs (%.1f ops/s), %d failures",
		ops, wallClock.Seconds(), opsPerSec, fails)
	t.Logf("[system] CPU=%.1f%%  MemΔ=%.1fMB  PeakMem=%.1fMB  GoroutineΔ=%.0f  GCSΔ=%.0f",
		delta.CPUUsagePercent, delta.MemoryDeltaMB, delta.PeakResidentMB,
		delta.GoroutineDelta, delta.GCSAPICallsDelta)

	errorRate := float64(fails) / float64(ops) * 100
	if errorRate > 10.0 {
		t.Fatalf("Control plane error rate too high: %.2f%% (%d/%d)", errorRate, fails, ops)
	}
}
