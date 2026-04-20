package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
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

// ---------------------------------------------------------------------------
// Configuration (overridable via environment variables)
// ---------------------------------------------------------------------------

func getBenchConcurrency() int {
	if v := os.Getenv("BENCH_CONCURRENCY"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			return n
		}
	}
	return 50
}

func getBenchDurationSec() int {
	if v := os.Getenv("BENCH_DURATION_SEC"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			return n
		}
	}
	return 30
}

// ---------------------------------------------------------------------------
// Report types
// ---------------------------------------------------------------------------

// BenchmarkReport holds the aggregate benchmark results with system metrics.
type BenchmarkReport struct {
	Timestamp     string            `json:"timestamp"`
	ProxyEndpoint string            `json:"proxy_endpoint"`
	ProxyNodePool string            `json:"proxy_node_pool,omitempty"`
	Config        BenchmarkConfig   `json:"config"`
	Results       []BenchmarkResult `json:"results"`
}

type BenchmarkConfig struct {
	Concurrency int `json:"concurrency"`
	DurationSec int `json:"duration_sec"`
}

// BenchmarkResult holds a single benchmark's statistics.
type BenchmarkResult struct {
	Name        string      `json:"name"`
	PayloadSize string      `json:"payload_size"`
	Concurrency int         `json:"concurrency"`
	DurationSec float64     `json:"duration_sec"`
	TotalOps    int64       `json:"total_ops"`
	Errors      int64       `json:"errors"`
	OpsPerSec   float64     `json:"ops_per_sec"`
	P50Ms       float64     `json:"p50_ms"`
	P95Ms       float64     `json:"p95_ms"`
	P99Ms       float64     `json:"p99_ms"`
	AvgMs       float64     `json:"avg_ms"`
	MaxMs       float64     `json:"max_ms"`
	System      SystemDelta `json:"system_metrics"`
	PodMetrics  PodMetrics  `json:"pod_metrics"`
}

// ---------------------------------------------------------------------------
// Latency helpers
// ---------------------------------------------------------------------------

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func avgDuration(sorted []time.Duration) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return total / time.Duration(len(sorted))
}

func durationMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// ---------------------------------------------------------------------------
// Concurrent load runner
// ---------------------------------------------------------------------------

// loadResult holds raw results from a concurrent load test.
type loadResult struct {
	latencies []time.Duration
	totalOps  int64
	errors    int64
}

// cleanupPutKeys deletes the given keys in parallel (best-effort, no error
// propagation).  This runs AFTER the benchmark measurement is complete so
// the DELETE traffic does not interfere with PutObject throughput numbers.
func cleanupPutKeys(client *s3.Client, bucket string, keys []string, workers int) {
	if len(keys) == 0 {
		return
	}
	ch := make(chan string, len(keys))
	for _, k := range keys {
		ch <- k
	}
	close(ch)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range ch {
				client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
				})
			}
		}()
	}
	wg.Wait()
}

// runConcurrentLoad runs fn across N goroutines for the specified duration,
// collecting per-operation latencies from all goroutines.
func runConcurrentLoad(concurrency int, duration time.Duration, fn func() error) loadResult {
	var (
		mu        sync.Mutex
		allLats   []time.Duration
		totalOps  atomic.Int64
		totalErrs atomic.Int64
	)

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup

	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var localLats []time.Duration

			for time.Now().Before(deadline) {
				start := time.Now()
				if err := fn(); err != nil {
					totalErrs.Add(1)
				} else {
					localLats = append(localLats, time.Since(start))
				}
				totalOps.Add(1)
			}

			mu.Lock()
			allLats = append(allLats, localLats...)
			mu.Unlock()
		}()
	}

	wg.Wait()

	sort.Slice(allLats, func(i, j int) bool { return allLats[i] < allLats[j] })

	return loadResult{
		latencies: allLats,
		totalOps:  totalOps.Load(),
		errors:    totalErrs.Load(),
	}
}

// toBenchmarkResult constructs a BenchmarkResult from load results + system delta.
func toBenchmarkResult(name, payloadSize string, concurrency int, lr loadResult, wallClock time.Duration, delta SystemDelta, pod PodMetrics) BenchmarkResult {
	opsPerSec := 0.0
	if wallClock.Seconds() > 0 {
		opsPerSec = float64(len(lr.latencies)) / wallClock.Seconds()
	}

	maxMs := 0.0
	if len(lr.latencies) > 0 {
		maxMs = durationMs(lr.latencies[len(lr.latencies)-1])
	}

	return BenchmarkResult{
		Name:        name,
		PayloadSize: payloadSize,
		Concurrency: concurrency,
		DurationSec: wallClock.Seconds(),
		TotalOps:    lr.totalOps,
		Errors:      lr.errors,
		OpsPerSec:   opsPerSec,
		P50Ms:       durationMs(percentile(lr.latencies, 0.50)),
		P95Ms:       durationMs(percentile(lr.latencies, 0.95)),
		P99Ms:       durationMs(percentile(lr.latencies, 0.99)),
		AvgMs:       durationMs(avgDuration(lr.latencies)),
		MaxMs:       maxMs,
		System:      delta,
		PodMetrics:  pod,
	}
}

// collectPodMetrics queries Prometheus for pod-level signals over the benchmark
// interval. It expands the window by 30s on each side so the rate(..[60s])
// windows have enough data. On any error (or if the collector is disabled) it
// returns a zero-valued PodMetrics; the benchmark must not fail because
// Prometheus is unreachable.
func collectPodMetrics(pc *PrometheusCollector, start, end time.Time) PodMetrics {
	if pc == nil {
		return PodMetrics{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	padded := 30 * time.Second
	pod, err := pc.Collect(ctx, start.Add(-padded), end.Add(padded))
	if err != nil {
		fmt.Printf("  [prometheus] collection failed: %v\n", err)
		return PodMetrics{}
	}
	return pod
}

// ---------------------------------------------------------------------------
// Payload helpers
// ---------------------------------------------------------------------------

type payloadTier struct {
	Name string
	Size int
}

var payloadTiers = []payloadTier{
	{"1KB", 1024},
	{"100KB", 100 * 1024},
	{"1MB", 1024 * 1024},
	{"10MB", 10 * 1024 * 1024},
}

func makePayload(size int) string {
	return strings.Repeat("X", size)
}

// ---------------------------------------------------------------------------
// Benchmark suite
// ---------------------------------------------------------------------------

// TestBenchmarkSuite runs all benchmarks with concurrent load and system metrics.
func TestBenchmarkSuite(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	concurrency := getBenchConcurrency()
	durationSec := getBenchDurationSec()
	duration := time.Duration(durationSec) * time.Second

	mc := NewMetricsCollector(testEnv.ProxyEndpoint)

	pc := NewPrometheusCollector()
	if v := os.Getenv("PROMETHEUS_URL"); v == "" {
		t.Logf("PROMETHEUS_URL not set, falling back to default in-cluster URL")
	}
	t.Logf("Prometheus URL: %s", pc.baseURL)

	// Cross-namespace DNS + reachability probe. If Prometheus is unreachable
	// (wrong service name, network policy, etc.) we log a detailed warning
	// so CI logs make the root cause obvious, but we do NOT fail the benchmark.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := pc.Ping(pingCtx); err != nil {
		t.Logf("WARN Prometheus pre-flight check failed: %v (pod metrics will be zero)", err)
	}
	pingCancel()

	report := BenchmarkReport{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ProxyEndpoint: testEnv.ProxyEndpoint,
		ProxyNodePool: os.Getenv("PROXY_NODE_POOL"),
		Config: BenchmarkConfig{
			Concurrency: concurrency,
			DurationSec: durationSec,
		},
	}

	t.Logf("Benchmark config: concurrency=%d, duration=%ds", concurrency, durationSec)
	t.Log("")

	// =======================================================================
	// 1. Multi-tier PutObject load test
	// =======================================================================
	for _, tier := range payloadTiers {
		t.Logf("=== PutObject %s (%d concurrent, %ds) ===", tier.Name, concurrency, durationSec)
		payload := makePayload(tier.Size)

		// Collect keys for post-benchmark cleanup instead of fire-and-forget
		// DELETEs that would share the writeProxy HTTP/2 connection and
		// interfere with PUT throughput measurement.
		var putKeysMu sync.Mutex
		var putKeys []string

		before, _ := mc.Snapshot()
		start := time.Now()

		lr := runConcurrentLoad(concurrency, duration, func() error {
			key := fmt.Sprintf("%sbench-put-%s-%d", testEnv.TestPrefix, tier.Name, time.Now().UnixNano())
			_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
				Body:   strings.NewReader(payload),
			})
			if err == nil {
				putKeysMu.Lock()
				putKeys = append(putKeys, key)
				putKeysMu.Unlock()
			}
			return err
		})

		wallClock := time.Since(start)
		end := time.Now()
		after, _ := mc.Snapshot()
		delta := ComputeDelta(before, after, wallClock)
		pod := collectPodMetrics(pc, start, end)

		result := toBenchmarkResult("PutObject_"+tier.Name, tier.Name, concurrency, lr, wallClock, delta, pod)
		report.Results = append(report.Results, result)

		printBenchResult(t, result)

		// Deferred batch cleanup — runs AFTER measurement is complete.
		t.Logf("  Cleaning up %d objects...", len(putKeys))
		cleanupPutKeys(client, bucket, putKeys, concurrency)
		t.Logf("  Cleanup done.")
	}

	// =======================================================================
	// 2. Multi-tier GetObject load test
	// =======================================================================
	for _, tier := range payloadTiers {
		t.Logf("=== GetObject %s (%d concurrent, %ds) ===", tier.Name, concurrency, durationSec)

		// Seed one object for reads
		seedKey := fmt.Sprintf("%sbench-get-seed-%s", testEnv.TestPrefix, tier.Name)
		payload := makePayload(tier.Size)
		_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(seedKey),
			Body:   strings.NewReader(payload),
		})
		if err != nil {
			t.Logf("  [skip] Failed to seed object for GetObject %s: %v", tier.Name, err)
			continue
		}
		t.Cleanup(func() { Cleanup(t, client, bucket, seedKey) })

		before, _ := mc.Snapshot()
		start := time.Now()

		lr := runConcurrentLoad(concurrency, duration, func() error {
			out, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(seedKey),
			})
			if err != nil {
				return err
			}
			io.Copy(io.Discard, out.Body)
			out.Body.Close()
			return nil
		})

		wallClock := time.Since(start)
		end := time.Now()
		after, _ := mc.Snapshot()
		delta := ComputeDelta(before, after, wallClock)
		pod := collectPodMetrics(pc, start, end)

		result := toBenchmarkResult("GetObject_"+tier.Name, tier.Name, concurrency, lr, wallClock, delta, pod)
		report.Results = append(report.Results, result)

		printBenchResult(t, result)
	}

	// =======================================================================
	// 3. Full CRUD cycle load test (1KB)
	// =======================================================================
	t.Logf("=== PutGetDelete CRUD Cycle (%d concurrent, %ds) ===", concurrency, durationSec)

	before, _ := mc.Snapshot()
	start := time.Now()

	crudLR := runConcurrentLoad(concurrency, duration, func() error {
		key := fmt.Sprintf("%sbench-crud-%d", testEnv.TestPrefix, time.Now().UnixNano())
		// Put
		_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   strings.NewReader(strings.Repeat("Y", 1024)),
		})
		if err != nil {
			return err
		}
		// Get
		out, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return err
		}
		io.Copy(io.Discard, out.Body)
		out.Body.Close()
		// Delete
		_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		return err
	})

	wallClock := time.Since(start)
	crudEnd := time.Now()
	after, _ := mc.Snapshot()
	delta := ComputeDelta(before, after, wallClock)
	crudPod := collectPodMetrics(pc, start, crudEnd)

	crudResult := toBenchmarkResult("PutGetDelete_CRUD", "1KB", concurrency, crudLR, wallClock, delta, crudPod)
	report.Results = append(report.Results, crudResult)
	printBenchResult(t, crudResult)

	// =======================================================================
	// 4. Control Plane: PutBucketLifecycle load test
	// =======================================================================
	t.Logf("=== PutBucketLifecycle (%d concurrent, %ds) ===", concurrency, durationSec)
	t.Cleanup(func() {
		client.DeleteBucketLifecycle(context.TODO(), &s3.DeleteBucketLifecycleInput{
			Bucket: aws.String(bucket),
		})
	})

	before, _ = mc.Snapshot()
	start = time.Now()

	lcLR := runConcurrentLoad(concurrency, duration, func() error {
		_, err := client.PutBucketLifecycleConfiguration(context.TODO(), &s3.PutBucketLifecycleConfigurationInput{
			Bucket: aws.String(bucket),
			LifecycleConfiguration: &types.BucketLifecycleConfiguration{
				Rules: []types.LifecycleRule{
					{
						ID:     aws.String("bench-rule"),
						Status: types.ExpirationStatusEnabled,
						Filter: &types.LifecycleRuleFilter{Prefix: aws.String("bench/")},
						Expiration: &types.LifecycleExpiration{
							Days: aws.Int32(90),
						},
					},
				},
			},
		})
		return err
	})

	wallClock = time.Since(start)
	lcEnd := time.Now()
	after, _ = mc.Snapshot()
	delta = ComputeDelta(before, after, wallClock)
	lcPod := collectPodMetrics(pc, start, lcEnd)

	lcResult := toBenchmarkResult("PutBucketLifecycle", "N/A", concurrency, lcLR, wallClock, delta, lcPod)
	report.Results = append(report.Results, lcResult)
	printBenchResult(t, lcResult)

	// =======================================================================
	// 5. Write JSON report
	// =======================================================================
	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal benchmark report: %v", err)
	}

	reportPath := "benchmark_report.json"
	if err := os.WriteFile(reportPath, reportJSON, 0644); err != nil {
		t.Logf("Warning: failed to write report to %s: %v", reportPath, err)
	} else {
		t.Logf("Benchmark report written to %s", reportPath)
	}

	// Also emit JSON to stdout with markers for easy extraction from container logs
	fmt.Println("BENCHMARK_JSON_START")
	fmt.Println(string(reportJSON))
	fmt.Println("BENCHMARK_JSON_END")

	// Print final summary table
	fmt.Println()
	fmt.Println("================================================================================")
	fmt.Println("  Benchmark Summary")
	fmt.Printf("  Concurrency: %d | Duration per test: %ds\n", concurrency, durationSec)
	fmt.Println("================================================================================")
	fmt.Printf("  %-25s %6s %6s %8s %8s %8s %8s %8s %6s\n",
		"Name", "Size", "Ops", "ops/s", "p50(ms)", "p95(ms)", "p99(ms)", "max(ms)", "Errs")
	fmt.Println("  " + strings.Repeat("-", 94))
	for _, r := range report.Results {
		fmt.Printf("  %-25s %6s %6d %8.1f %8.1f %8.1f %8.1f %8.1f %6d\n",
			r.Name, r.PayloadSize, r.TotalOps, r.OpsPerSec, r.P50Ms, r.P95Ms, r.P99Ms, r.MaxMs, r.Errors)
	}
	fmt.Println("================================================================================")
	fmt.Println()

	// Print system metrics summary
	fmt.Println("  System Metrics (per benchmark):")
	fmt.Printf("  %-25s %8s %10s %10s %10s %10s\n",
		"Name", "CPU(%)", "Mem\u0394(MB)", "PeakMem", "Gortn\u0394", "HTTP\u0394")
	fmt.Println("  " + strings.Repeat("-", 78))
	for _, r := range report.Results {
		fmt.Printf("  %-25s %8.1f %10.1f %10.1f %10.0f %10.0f\n",
			r.Name, r.System.CPUUsagePercent, r.System.MemoryDeltaMB,
			r.System.PeakResidentMB, r.System.GoroutineDelta, r.System.HTTPRequestsDelta)
	}
	fmt.Println("================================================================================")

	// Print pod-level metrics from Prometheus
	fmt.Println()
	fmt.Println("  Pod Metrics (from Prometheus, per benchmark):")
	fmt.Printf("  %-25s %12s %12s %12s %12s %10s %10s\n",
		"Name", "NetRx(MB/s)", "NetTx(MB/s)", "CPU(cores)", "Mem(MB)", "QPS", "Goroutines")
	fmt.Println("  " + strings.Repeat("-", 98))
	for _, r := range report.Results {
		pm := r.PodMetrics
		fmt.Printf("  %-25s %12s %12s %12s %12s %10s %10s\n",
			r.Name,
			statsMBs(pm.NetRxBps),
			statsMBs(pm.NetTxBps),
			statsStr(pm.CPUCores, 2),
			statsStr(pm.MemoryMB, 1),
			statsStr(pm.HTTPReqRate, 1),
			statsStr(pm.Goroutines, 0))
	}
	fmt.Println("================================================================================")
}

// statsStr formats max/avg for a Stats value (compact).
func statsStr(s Stats, prec int) string {
	return fmt.Sprintf("%.*f/%.*f", prec, s.Max, prec, s.Avg)
}

// statsMBs formats Stats in MB/s (from bytes/s).
func statsMBs(s Stats) string {
	return fmt.Sprintf("%.2f/%.2f", s.Max/(1024*1024), s.Avg/(1024*1024))
}

func printBenchResult(t *testing.T, r BenchmarkResult) {
	t.Logf("  ops=%d  ops/s=%.1f  p50=%.1fms  p95=%.1fms  p99=%.1fms  max=%.1fms  errors=%d",
		r.TotalOps, r.OpsPerSec, r.P50Ms, r.P95Ms, r.P99Ms, r.MaxMs, r.Errors)
	t.Logf("  [system] CPU=%.1f%%  Mem\u0394=%.1fMB  PeakMem=%.1fMB  Goroutine\u0394=%.0f  HTTP\u0394=%.0f",
		r.System.CPUUsagePercent, r.System.MemoryDeltaMB,
		r.System.PeakResidentMB, r.System.GoroutineDelta, r.System.HTTPRequestsDelta)
	pm := r.PodMetrics
	t.Logf("  [pod] NetRx max=%.2fMB/s avg=%.2fMB/s  NetTx max=%.2fMB/s avg=%.2fMB/s  CPU max=%.2f avg=%.2f  Mem max=%.1fMB",
		pm.NetRxBps.Max/(1024*1024), pm.NetRxBps.Avg/(1024*1024),
		pm.NetTxBps.Max/(1024*1024), pm.NetTxBps.Avg/(1024*1024),
		pm.CPUCores.Max, pm.CPUCores.Avg, pm.MemoryMB.Max)
	t.Log("")
}
