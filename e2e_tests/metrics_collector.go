// Package e2e provides shared test infrastructure for E2E acceptance tests.
package e2e

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemSnapshot captures proxy-side resource metrics at a point in time.
type SystemSnapshot struct {
	Timestamp         string  `json:"timestamp"`
	CPUSecondsTotal   float64 `json:"cpu_seconds_total"`
	ResidentMemoryMB  float64 `json:"resident_memory_mb"`
	Goroutines        float64 `json:"goroutines"`
	HeapAllocMB       float64 `json:"heap_alloc_mb"`
	HeapInUseMB       float64 `json:"heap_in_use_mb"`
	OpenFDs           float64 `json:"open_fds"`
	HTTPRequestsTotal float64 `json:"http_requests_total"`
	GCSAPICalls       float64 `json:"gcs_api_calls_total"`
}

// SystemDelta represents the change between two snapshots.
type SystemDelta struct {
	DurationSec       float64        `json:"duration_sec"`
	CPUUsagePercent   float64        `json:"cpu_usage_percent"` // CPU delta / wall-clock delta * 100
	MemoryDeltaMB     float64        `json:"memory_delta_mb"`   // resident memory change
	PeakResidentMB    float64        `json:"peak_resident_mb"`  // max of before/after
	GoroutineDelta    float64        `json:"goroutine_delta"`
	HeapAllocDeltaMB  float64        `json:"heap_alloc_delta_mb"`
	HTTPRequestsDelta float64        `json:"http_requests_delta"`
	GCSAPICallsDelta  float64        `json:"gcs_api_calls_delta"`
	Before            SystemSnapshot `json:"before"`
	After             SystemSnapshot `json:"after"`
}

// MetricsCollector fetches Prometheus metrics from the proxy's /metrics endpoint.
type MetricsCollector struct {
	metricsURL string
	client     *http.Client
}

// NewMetricsCollector creates a collector pointed at the proxy endpoint.
func NewMetricsCollector(proxyEndpoint string) *MetricsCollector {
	return &MetricsCollector{
		metricsURL: strings.TrimRight(proxyEndpoint, "/") + "/metrics",
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Snapshot fetches current metrics and returns a SystemSnapshot.
func (mc *MetricsCollector) Snapshot() (SystemSnapshot, error) {
	resp, err := mc.client.Get(mc.metricsURL)
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("failed to fetch metrics: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("failed to read metrics body: %w", err)
	}

	text := string(body)
	snap := SystemSnapshot{
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
		CPUSecondsTotal:   parseMetric(text, "process_cpu_seconds_total"),
		ResidentMemoryMB:  parseMetric(text, "process_resident_memory_bytes") / (1024 * 1024),
		Goroutines:        parseMetric(text, "go_goroutines"),
		HeapAllocMB:       parseMetric(text, "go_memstats_alloc_bytes") / (1024 * 1024),
		HeapInUseMB:       parseMetric(text, "go_memstats_heap_inuse_bytes") / (1024 * 1024),
		OpenFDs:           parseMetric(text, "process_open_fds"),
		HTTPRequestsTotal: sumCounterVec(text, "s3proxy_http_requests_total"),
		GCSAPICalls:       sumHistogramCount(text, "s3proxy_gcs_api_duration_seconds"),
	}
	return snap, nil
}

// ComputeDelta calculates the difference between two snapshots.
func ComputeDelta(before, after SystemSnapshot, wallClock time.Duration) SystemDelta {
	durSec := wallClock.Seconds()
	cpuDelta := after.CPUSecondsTotal - before.CPUSecondsTotal
	cpuPercent := 0.0
	if durSec > 0 {
		cpuPercent = (cpuDelta / durSec) * 100
	}

	return SystemDelta{
		DurationSec:       math.Round(durSec*100) / 100,
		CPUUsagePercent:   math.Round(cpuPercent*100) / 100,
		MemoryDeltaMB:     math.Round((after.ResidentMemoryMB-before.ResidentMemoryMB)*100) / 100,
		PeakResidentMB:    math.Max(before.ResidentMemoryMB, after.ResidentMemoryMB),
		GoroutineDelta:    after.Goroutines - before.Goroutines,
		HeapAllocDeltaMB:  math.Round((after.HeapAllocMB-before.HeapAllocMB)*100) / 100,
		HTTPRequestsDelta: after.HTTPRequestsTotal - before.HTTPRequestsTotal,
		GCSAPICallsDelta:  after.GCSAPICalls - before.GCSAPICalls,
		Before:            before,
		After:             after,
	}
}

// ---------------------------------------------------------------------------
// Network throughput sampling
// ---------------------------------------------------------------------------

// NetworkStats holds per-benchmark network throughput statistics (MB/s)
// measured from /proc/net/dev on the benchmark pod.
type NetworkStats struct {
	AvgRxMBps float64 `json:"avg_rx_mbps"`
	MaxRxMBps float64 `json:"max_rx_mbps"`
	MinRxMBps float64 `json:"min_rx_mbps"`
	AvgTxMBps float64 `json:"avg_tx_mbps"`
	MaxTxMBps float64 `json:"max_tx_mbps"`
	MinTxMBps float64 `json:"min_tx_mbps"`
	Samples   int     `json:"samples"`
}

// NetworkSampler periodically reads /proc/net/dev and records per-second
// Rx/Tx throughput for all non-loopback interfaces combined.
type NetworkSampler struct {
	interval  time.Duration
	rxSamples []float64
	txSamples []float64
	mu        sync.Mutex
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewNetworkSampler creates a sampler with the given poll interval.
func NewNetworkSampler(interval time.Duration) *NetworkSampler {
	return &NetworkSampler{
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start begins background sampling. Returns immediately.
func (ns *NetworkSampler) Start() {
	go func() {
		defer close(ns.doneCh)

		prevRx, prevTx, err := readNetworkBytes()
		if err != nil {
			return // /proc/net/dev not available (non-Linux environment)
		}
		prevTime := time.Now()

		ticker := time.NewTicker(ns.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ns.stopCh:
				return
			case t := <-ticker.C:
				rx, tx, err := readNetworkBytes()
				if err != nil {
					continue
				}
				elapsed := t.Sub(prevTime).Seconds()
				if elapsed > 0 && rx >= prevRx && tx >= prevTx {
					rxMBps := float64(rx-prevRx) / elapsed / (1024 * 1024)
					txMBps := float64(tx-prevTx) / elapsed / (1024 * 1024)
					ns.mu.Lock()
					ns.rxSamples = append(ns.rxSamples, rxMBps)
					ns.txSamples = append(ns.txSamples, txMBps)
					ns.mu.Unlock()
				}
				prevRx, prevTx = rx, tx
				prevTime = t
			}
		}
	}()
}

// Stop terminates sampling and returns the collected statistics.
func (ns *NetworkSampler) Stop() NetworkStats {
	close(ns.stopCh)
	<-ns.doneCh

	ns.mu.Lock()
	defer ns.mu.Unlock()
	return computeNetworkStats(ns.rxSamples, ns.txSamples)
}

// readNetworkBytes returns cumulative Rx/Tx bytes summed across all
// non-loopback interfaces by parsing /proc/net/dev.
func readNetworkBytes() (rxBytes, txBytes uint64, err error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(data), "\n")[2:] { // skip 2-line header
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colonIdx])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		rxBytes += rx
		txBytes += tx
	}
	return rxBytes, txBytes, nil
}

// computeNetworkStats derives min/max/avg from per-second throughput samples.
func computeNetworkStats(rxSamples, txSamples []float64) NetworkStats {
	n := len(rxSamples)
	if n == 0 {
		return NetworkStats{}
	}
	var rxSum, txSum float64
	rxMin, rxMax := rxSamples[0], rxSamples[0]
	txMin, txMax := txSamples[0], txSamples[0]
	for i, rx := range rxSamples {
		rxSum += rx
		if rx < rxMin {
			rxMin = rx
		}
		if rx > rxMax {
			rxMax = rx
		}
		tx := txSamples[i]
		txSum += tx
		if tx < txMin {
			txMin = tx
		}
		if tx > txMax {
			txMax = tx
		}
	}
	fn := float64(n)
	return NetworkStats{
		AvgRxMBps: math.Round(rxSum/fn*100) / 100,
		MaxRxMBps: math.Round(rxMax*100) / 100,
		MinRxMBps: math.Round(rxMin*100) / 100,
		AvgTxMBps: math.Round(txSum/fn*100) / 100,
		MaxTxMBps: math.Round(txMax*100) / 100,
		MinTxMBps: math.Round(txMin*100) / 100,
		Samples:   n,
	}
}

// parseMetric extracts a single gauge/counter value from Prometheus text format.
// Matches lines like: metric_name 123.45
func parseMetric(text, name string) float64 {
	// Match exact metric name (no labels) at start of line
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+([\d.eE+\-]+)`)
	match := re.FindStringSubmatch(text)
	if match == nil {
		return 0
	}
	v, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// sumCounterVec sums all label combinations of a counter metric.
// Matches lines like: metric_name{label="value",...} 123
func sumCounterVec(text, name string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\{[^}]*\}\s+([\d.eE+\-]+)`)
	matches := re.FindAllStringSubmatch(text, -1)
	total := 0.0
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			total += v
		}
	}
	return total
}

// sumHistogramCount sums _count entries of a histogram metric.
func sumHistogramCount(text, name string) float64 {
	countName := name + "_count"
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(countName) + `\{[^}]*\}\s+([\d.eE+\-]+)`)
	matches := re.FindAllStringSubmatch(text, -1)
	total := 0.0
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			total += v
		}
	}
	// Also try without labels
	total += parseMetric(text, countName)
	return total
}
