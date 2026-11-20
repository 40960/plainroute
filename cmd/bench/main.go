// SPDX-License-Identifier: AGPL-3.0-only

// plainroute-bench is a load generator for a running gateway. It sends requests,
// measures what the client sees, then reads the gateway's own numbers from its metrics
// page. It reads that page before and after the load: the upstream-attempt delta must not
// exceed the number of client requests this client issued. That is the no-amplification
// invariant checked under load, not only in a unit test. Gateway rejections and client
// disconnects are reported beside it, because they explain a difference between the
// request and attempt counters without being breaches of the bound.
//
// It is not a benchmark of a provider. It measures the gateway: what it adds to the
// path, and whether its counters come back to zero after the load stops.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type outcome struct {
	status   int
	latency  time.Duration
	ttft     time.Duration
	bytes    int64
	err      string
	streamed bool
}

type sample struct {
	name  string
	value float64
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080", "gateway base URL")
	model := flag.String("model", "bench", "model name to request")
	requests := flag.Int("requests", 500, "number of requests")
	concurrency := flag.Int("concurrency", 32, "workers")
	stream := flag.Bool("stream", true, "ask for a streamed response")
	timeout := flag.Duration("timeout", 30*time.Second, "per request timeout")
	metricsPath := flag.String("metrics", "/metrics", "metrics path on the gateway")
	maxAttempts := flag.Int("max-attempts-per-request", 1, "maximum upstream attempts a single client request may cause; raise it only when the configured fallback chain is enabled")
	flag.Parse()

	if *requests <= 0 || *concurrency <= 0 || *maxAttempts <= 0 {
		fmt.Fprintln(os.Stderr, "bench: requests, concurrency and max-attempts-per-request must be positive")
		os.Exit(1)
	}

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency,
			MaxIdleConnsPerHost: *concurrency,
		},
	}

	body := []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"bench"}]}`,
		*model, *stream))

	baseline, err := fetchMetrics(client, *url, *metricsPath)
	if err != nil {
		fmt.Printf("\ngateway metrics baseline: %v\n", err)
		return
	}

	results := make([]outcome, *requests)
	next := int64(0)
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= *requests {
					return
				}
				results[i] = doRequest(client, *url, body)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	printReport(results, elapsed, *requests, *concurrency)

	page, err := fetchMetrics(client, *url, *metricsPath)
	if err != nil {
		fmt.Printf("\ngateway metrics: %v\n", err)
		return
	}
	printGateway(baseline, page, *requests, *maxAttempts)
}

func doRequest(client *http.Client, url string, body []byte) outcome {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(url, "/")+"/v1/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return outcome{err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return outcome{err: err.Error(), latency: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()

	out := outcome{status: resp.StatusCode}
	out.streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if out.ttft == 0 {
				out.ttft = time.Since(start)
			}
			out.bytes += int64(n)
		}
		if err != nil {
			if err != io.EOF {
				out.err = err.Error()
			}
			break
		}
	}
	out.latency = time.Since(start)
	return out
}

func printReport(results []outcome, elapsed time.Duration, requests, concurrency int) {
	latencies := make([]time.Duration, 0, len(results))
	ttfts := make([]time.Duration, 0, len(results))
	status := map[string]int{}
	var bytes, errors, streamed int64

	for _, r := range results {
		if r.err != "" {
			errors++
			status["transport error"]++
			continue
		}
		status[statusClass(r.status)]++
		latencies = append(latencies, r.latency)
		if r.ttft > 0 {
			ttfts = append(ttfts, r.ttft)
		}
		if r.streamed {
			streamed++
		}
		bytes += r.bytes
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Printf("bench: %d requests, concurrency %d\n", requests, concurrency)
	fmt.Printf("wall: %v  requests/sec: %.1f\n", elapsed.Round(time.Millisecond),
		float64(requests)/elapsed.Seconds())

	if len(latencies) > 0 {
		fmt.Printf("latency: p50 %v  p95 %v  p99 %v  max %v\n",
			percentile(latencies, 0.50).Round(time.Millisecond),
			percentile(latencies, 0.95).Round(time.Millisecond),
			percentile(latencies, 0.99).Round(time.Millisecond),
			latencies[len(latencies)-1].Round(time.Millisecond))
	}
	if len(ttfts) > 0 {
		fmt.Printf("time to first byte: p50 %v  p95 %v\n",
			percentile(ttfts, 0.50).Round(time.Microsecond),
			percentile(ttfts, 0.95).Round(time.Microsecond))
	}

	fmt.Printf("statuses:")
	for _, name := range sortedKeys(status) {
		fmt.Printf(" %s=%d", name, status[name])
	}
	fmt.Printf("\nstreamed: %d  transport errors: %d  bytes received: %d\n", streamed, errors, bytes)

	fmt.Printf("client memory: heap %d KB  total alloc %d KB  gc %d  goroutines %d\n",
		mem.HeapAlloc/1024, mem.TotalAlloc/1024, mem.NumGC, runtime.NumGoroutine())
}

// printGateway reports the numbers the gateway keeps, as totals and as deltas from the
// baseline read before the load. The bound is on the upstream-attempt delta against the
// requests this client issued; a rejection or a disconnect can make the two counters drift
// apart without breaching it. Every gauge must be back at zero after the load stopped.
func printGateway(baselinePage, page string, clientRequestsIssued, maxAttemptsPerRequest int) {
	samples := parseExposition(page)

	wanted := []string{
		"plainroute_requests_total",
		"plainroute_upstream_attempts_total",
		"plainroute_active_requests",
		"plainroute_target_in_cooldown",
		"plainroute_client_disconnects_total",
		"plainroute_stream_aborts_total",
		"plainroute_gateway_rejected_total",
		"plainroute_in_flight_requests",
		"go_goroutines",
		"go_memstats_heap_alloc_bytes",
		"process_resident_memory_bytes",
	}

	fmt.Printf("\ngateway, read from its own metrics page:\n")
	totals, before, seen := map[string]float64{}, map[string]float64{}, map[string]int{}
	for _, s := range parseExposition(baselinePage) {
		before[s.name] += s.value
	}
	for _, s := range samples {
		totals[s.name] += s.value
		seen[s.name]++
	}

	for _, name := range wanted {
		if seen[name] == 0 {
			continue
		}
		unit := ""
		if strings.HasSuffix(name, "_bytes") {
			unit = fmt.Sprintf(" (%d KB)", int64(totals[name]/1024))
		}
		fmt.Printf("  %-36s %s (+%s)%s\n", name, trimNumber(totals[name]),
			trimNumber(totals[name]-before[name]), unit)
	}

	// This client knows how many requests it issued, and the baseline keeps traffic from
	// earlier runs out of the comparison. The bound is maxAttemptsPerRequest per request
	// issued: one for the default gateway, more only when an explicit fallback chain is
	// configured. The flag is never guessed from the gateway config.
	attempts := totals["plainroute_upstream_attempts_total"] - before["plainroute_upstream_attempts_total"]
	limit := float64(clientRequestsIssued * maxAttemptsPerRequest)
	verdict := "ok"
	if seen["plainroute_upstream_attempts_total"] == 0 {
		verdict = "FAIL: no upstream attempts counter on the metrics page"
	} else if attempts > limit {
		verdict = "FAIL: more upstream attempts than client requests issued times max-attempts-per-request"
	}
	fmt.Printf("  %-36s %d attempts since baseline for %d requests x %d allowed: %s\n",
		"no amplification", int64(attempts), clientRequestsIssued, maxAttemptsPerRequest, verdict)

	// These explain a difference between the two counters; they are not part of the bound.
	fmt.Printf("  %-36s +%d (auxiliary; answered from gateway state)\n", "gateway rejected",
		int64(totals["plainroute_gateway_rejected_total"]-before["plainroute_gateway_rejected_total"]))
	fmt.Printf("  %-36s +%d (auxiliary; client left before the response)\n", "client disconnects",
		int64(totals["plainroute_client_disconnects_total"]-before["plainroute_client_disconnects_total"]))

	gauges := []string{"plainroute_active_requests", "plainroute_in_flight_requests"}
	for _, name := range gauges {
		if seen[name] > 0 && totals[name] != 0 {
			fmt.Printf("  %-36s %s FAIL: gauge still %s after the load stopped\n",
				name, name, trimNumber(totals[name]))
		}
	}
}

func parseExposition(page string) []sample {
	out := make([]sample, 0, 256)
	for _, line := range strings.Split(page, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if i <= 0 {
			continue
		}
		name := line[:i]
		if j := strings.Index(name, "{"); j > 0 {
			name = name[:j]
		}
		var value float64
		if _, err := fmt.Sscanf(line[i+1:], "%g", &value); err != nil {
			continue
		}
		out = append(out, sample{name: name, value: value})
	}
	return out
}

func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func percentile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	i := int(q * float64(len(values)))
	if i >= len(values) {
		i = len(values) - 1
	}
	return values[i]
}

func fetchMetrics(client *http.Client, url, path string) (string, error) {
	resp, err := client.Get(strings.TrimRight(url, "/") + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func trimNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}
