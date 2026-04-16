// Pressure runner used by the multi-sdk-dns-cutting-tests workflow.
//
// Single-purpose main: drive AWS SDK Go V2 against the s3proxy endpoint for
// a fixed wall-clock duration with N concurrent workers, then write a JSON
// report. Designed to run inside a K8s Job so it sees the cluster-internal
// DNS view of `s3proxy.lb.local` (internal ALB in Phase A, CNAME->GCS in
// Phase C).
//
// Flags:
//
//	-op           get|put           (required)
//	-duration     2m                (default; Go duration syntax)
//	-concurrency  2                 (default)
//	-endpoint     http://s3proxy.lb.local:8080 (default)
//	-bucket       <required>
//	-prefix       dns-cut-<runid>/  (object key prefix)
//	-payload-bytes 1024             (default)
//	-output       /tmp/pressure_report.json    (default)
//	-sdk          go-v2             (label only, echoed into JSON)
//	-phase        A|C               (label only, echoed into JSON)
//
// HMAC creds picked up from env: GCS_HMAC_ACCESS, GCS_HMAC_SECRET.
//
// Output JSON shape (also printed between PRESSURE_JSON_START/END markers):
//
//	{
//	  "sdk": "go-v2", "op": "GET", "phase": "A",
//	  "duration_sec": 120.0, "total_requests": N, "success": N, "error": N,
//	  "error_rate": 0.0, "throughput_rps": 0.0,
//	  "latency_ms": {"p50":..,"p95":..,"p99":..,"max":..,"avg":..},
//	  "concurrency": 2, "payload_bytes": 1024,
//	  "started_at": "...", "ended_at": "...",
//	  "endpoint": "http://s3proxy.lb.local:8080"
//	}
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type latencyMs struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

type report struct {
	SDK           string    `json:"sdk"`
	Op            string    `json:"op"`
	Phase         string    `json:"phase"`
	DurationSec   float64   `json:"duration_sec"`
	TotalRequests int64     `json:"total_requests"`
	Success       int64     `json:"success"`
	Error         int64     `json:"error"`
	ErrorRate     float64   `json:"error_rate"`
	ThroughputRPS float64   `json:"throughput_rps"`
	LatencyMs     latencyMs `json:"latency_ms"`
	Concurrency   int       `json:"concurrency"`
	PayloadBytes  int       `json:"payload_bytes"`
	StartedAt     string    `json:"started_at"`
	EndedAt       string    `json:"ended_at"`
	Endpoint      string    `json:"endpoint"`
}

func percentileMs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("missing required env var %s", name)
	}
	return v
}

func main() {
	op := flag.String("op", "", "get|put (required)")
	durationStr := flag.String("duration", "2m", "wall-clock duration (Go duration syntax)")
	concurrency := flag.Int("concurrency", 2, "concurrent workers")
	endpoint := flag.String("endpoint", "http://s3proxy.lb.local:8080", "S3 endpoint URL")
	bucket := flag.String("bucket", "", "S3 bucket (required)")
	prefix := flag.String("prefix", "dns-cut/", "object key prefix")
	payloadBytes := flag.Int("payload-bytes", 1024, "request body size for PUT / seed object size for GET")
	outputPath := flag.String("output", "/tmp/pressure_report.json", "JSON report path")
	sdkLabel := flag.String("sdk", "go-v2", "SDK label echoed into report")
	phase := flag.String("phase", "A", "phase label echoed into report")
	flag.Parse()

	*op = strings.ToUpper(*op)
	if *op != "GET" && *op != "PUT" {
		log.Fatalf("-op must be get or put, got %q", *op)
	}
	if *bucket == "" {
		log.Fatal("-bucket is required")
	}
	if !strings.HasSuffix(*prefix, "/") {
		*prefix += "/"
	}
	duration, err := time.ParseDuration(*durationStr)
	if err != nil {
		log.Fatalf("invalid -duration: %v", err)
	}

	ak := mustEnv("GCS_HMAC_ACCESS")
	sk := mustEnv("GCS_HMAC_SECRET")

	creds := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk, Source: "pressure"}, nil
	})
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(creds),
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 60 * time.Second,
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(*endpoint)
		o.HTTPClient = httpClient
	})

	payload := make([]byte, *payloadBytes)
	if _, err := rand.Read(payload); err != nil {
		log.Fatalf("seed payload: %v", err)
	}

	log.Printf("pressure: sdk=%s op=%s phase=%s duration=%s concurrency=%d payload=%dB endpoint=%s bucket=%s",
		*sdkLabel, *op, *phase, duration, *concurrency, *payloadBytes, *endpoint, *bucket)

	// Seed for GET
	var seedKey string
	if *op == "GET" {
		seedKey = fmt.Sprintf("%spressure-seed-%s-%s-%d", *prefix, *sdkLabel, *phase, time.Now().UnixNano())
		if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket:        aws.String(*bucket),
			Key:           aws.String(seedKey),
			Body:          bytes.NewReader(payload),
			ContentLength: aws.Int64(int64(len(payload))),
		}); err != nil {
			log.Fatalf("seed put: %v", err)
		}
		defer func() {
			_, _ = client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
				Bucket: aws.String(*bucket),
				Key:    aws.String(seedKey),
			})
		}()
	}

	doOnce := func() error {
		switch *op {
		case "GET":
			out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: aws.String(*bucket),
				Key:    aws.String(seedKey),
			})
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(io.Discard, out.Body)
			out.Body.Close()
			return copyErr
		case "PUT":
			key := fmt.Sprintf("%spressure-put-%s-%s-%d", *prefix, *sdkLabel, *phase, time.Now().UnixNano())
			_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
				Bucket:        aws.String(*bucket),
				Key:           aws.String(key),
				Body:          bytes.NewReader(payload),
				ContentLength: aws.Int64(int64(len(payload))),
			})
			if err == nil {
				go func(k string) {
					_, _ = client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
						Bucket: aws.String(*bucket),
						Key:    aws.String(k),
					})
				}(key)
			}
			return err
		}
		return nil
	}

	var (
		mu          sync.Mutex
		latencies   []time.Duration
		total       atomic.Int64
		errs        atomic.Int64
		firstErrMu  sync.Mutex
		firstErrStr string
		errSamples  atomic.Int64
	)
	startWall := time.Now().UTC()
	t0 := time.Now()
	deadline := t0.Add(duration)

	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 1024)
			for time.Now().Before(deadline) {
				start := time.Now()
				err := doOnce()
				total.Add(1)
				if err != nil {
					errs.Add(1)
					if errSamples.Add(1) <= 5 {
						log.Printf("err sample: %v", err)
					}
					firstErrMu.Lock()
					if firstErrStr == "" {
						firstErrStr = err.Error()
					}
					firstErrMu.Unlock()
				} else {
					local = append(local, time.Since(start))
				}
			}
			mu.Lock()
			latencies = append(latencies, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	wall := time.Since(t0)
	endWall := time.Now().UTC()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	totalReqs := total.Load()
	errCount := errs.Load()
	successCount := int64(len(latencies))
	errRate := 0.0
	if totalReqs > 0 {
		errRate = float64(errCount) / float64(totalReqs)
	}
	rps := 0.0
	if wall.Seconds() > 0 {
		rps = float64(successCount) / wall.Seconds()
	}
	maxMs := 0.0
	if len(latencies) > 0 {
		maxMs = float64(latencies[len(latencies)-1].Microseconds()) / 1000.0
	}
	avgMs := 0.0
	if len(latencies) > 0 {
		var sum time.Duration
		for _, d := range latencies {
			sum += d
		}
		avgMs = float64(sum.Microseconds()) / 1000.0 / float64(len(latencies))
	}

	r := report{
		SDK:           *sdkLabel,
		Op:            *op,
		Phase:         *phase,
		DurationSec:   wall.Seconds(),
		TotalRequests: totalReqs,
		Success:       successCount,
		Error:         errCount,
		ErrorRate:     errRate,
		ThroughputRPS: rps,
		LatencyMs: latencyMs{
			P50: percentileMs(latencies, 0.50),
			P95: percentileMs(latencies, 0.95),
			P99: percentileMs(latencies, 0.99),
			Max: maxMs,
			Avg: avgMs,
		},
		Concurrency:  *concurrency,
		PayloadBytes: *payloadBytes,
		StartedAt:    startWall.Format(time.RFC3339),
		EndedAt:      endWall.Format(time.RFC3339),
		Endpoint:     *endpoint,
	}

	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(*outputPath, buf, 0644); err != nil {
		log.Printf("warn: write %s failed: %v", *outputPath, err)
	}

	// Markers for kubectl-logs extraction.
	fmt.Println("PRESSURE_JSON_START")
	fmt.Println(string(buf))
	fmt.Println("PRESSURE_JSON_END")

	log.Printf("done: total=%d success=%d error=%d rps=%.2f p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms",
		totalReqs, successCount, errCount, rps, r.LatencyMs.P50, r.LatencyMs.P95, r.LatencyMs.P99, r.LatencyMs.Max)
}
