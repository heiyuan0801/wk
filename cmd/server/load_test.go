package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Opt-in, loopback-only load test. It uses synthetic accounts and real HTTP
// connections on both sides of the proxy; no credentials or external services.
func TestProxyLoad(t *testing.T) {
	if os.Getenv("WK_LOADTEST") != "1" {
		t.Skip("set WK_LOADTEST=1 to run loopback load tests")
	}
	concurrency := os.Getenv("WK_LOAD_CONCURRENCY")
	if concurrency == "" {
		concurrency = "1,10,30,100,300,450"
	}
	duration := 3 * time.Second
	if raw := os.Getenv("WK_LOAD_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			t.Fatal("invalid WK_LOAD_DURATION")
		}
	}
	delay := 100 * time.Millisecond
	if raw := os.Getenv("WK_LOAD_DELAY"); raw != "" {
		var err error
		delay, err = time.ParseDuration(raw)
		if err != nil || delay < 0 {
			t.Fatal("invalid WK_LOAD_DELAY")
		}
	}
	for _, raw := range strings.Split(concurrency, ",") {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			t.Fatal("concurrency must be 1..1000")
		}
		t.Run(raw, func(t *testing.T) { runProxyLoad(t, n, duration, delay) })
	}
}

func runProxyLoad(t *testing.T, concurrency int, duration, delay time.Duration) {
	intOption := func(key string, fallback int) int {
		if raw := os.Getenv(key); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || v > 1000 {
				t.Fatalf("%s must be 1..1000", key)
			}
			return v
		}
		return fallback
	}
	accounts := intOption("WK_LOAD_ACCOUNTS", 100)
	perAccount := intOption("WK_LOAD_PER_ACCOUNT", 3)
	var active, peak atomic.Int64
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "load-id-unchanged")
		for i := 0; i < 4; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay / 4):
			}
			fmt.Fprint(w, "data: {\"id\":\"load-id-unchanged\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"load-ok\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"id\":\"load-id-unchanged\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n")
	}))
	defer mock.Close()
	up := upstream.New()
	up.ChatBaseCN = mock.URL
	defer up.HTTP.CloseIdleConnections()
	p := pool.New("") // pool state-file IO is excluded; SQLite writes are enabled
	p.SetMaxInFlight(perAccount)
	for i := 0; i < accounts; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i), ExpiresAt: time.Now().Add(time.Hour).Unix()})
	}
	var db metricsstore.Backend
	var err error
	if dsn := os.Getenv("WK_LOAD_POSTGRES_DSN"); dsn != "" {
		db, err = metricsstore.OpenPostgres(dsn, 32, 16, 30*time.Minute, 5*time.Minute)
	} else {
		db, err = metricsstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	adapter := metricsAdapter{store: db}
	cfg := server.Config{Pool: p, Upstream: up, MetricsStore: adapter, RequestLogStore: adapter,
		CompletionStore: adapter,
		Session:         session.New(session.Config{Available: p.AvailableUIDs}),
		CreditPolicy:    server.CreditPolicy{InputPer1K: 1, OutputPer1K: 1}}
	proxy := httptest.NewServer(server.NewHandler(cfg))
	defer proxy.Close()
	transport := &http.Transport{MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	stream := os.Getenv("WK_LOAD_STREAM") != "false"
	body, _ := json.Marshal(map[string]any{"model": "mock-model", "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 1024)}}})
	var mu sync.Mutex
	var latencies []float64
	statuses := map[int]int{}
	var attempts atomic.Int64
	runtime.GC()
	var peakHeap atomic.Uint64
	stopSample := make(chan struct{})
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peakHeap.Load() {
					peakHeap.Store(m.HeapAlloc)
				}
			}
		}
	}()
	start := time.Now()
	deadline := start.Add(duration)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-gate
			for time.Now().Before(deadline) {
				began := time.Now()
				req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Conversation-Id", fmt.Sprint(worker))
				resp, err := client.Do(req)
				code := 0
				if err == nil {
					data, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					code = resp.StatusCode
					if readErr != nil || (code == 200 && (!strings.Contains(string(data), "load-ok") || !strings.Contains(string(data), "load-id-unchanged") || (stream && !strings.Contains(string(data), "[DONE]")))) {
						code = -1
					}
				}
				attempts.Add(1)
				mu.Lock()
				statuses[code]++
				if code == 200 {
					latencies = append(latencies, float64(time.Since(began).Microseconds())/1000)
				}
				mu.Unlock()
				if code != 200 {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	close(stopSample)
	<-sampleDone
	// A drained server must have persisted every finished request, including failures.
	snapshot, err := db.Snapshot()
	if err != nil || snapshot.Requests != attempts.Load() || snapshot.Successes != int64(len(latencies)) || snapshot.InputTokens != int64(len(latencies)*10) {
		t.Fatalf("metrics=%+v attempts=%d err=%v", snapshot, attempts.Load(), err)
	}
	sort.Float64s(latencies)
	quantile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*p)]
	}
	if peak.Load() > int64(accounts*perAccount) {
		t.Fatal("upstream concurrency exceeded account limits")
	}
	if concurrency <= accounts*perAccount && int64(statuses[http.StatusOK]) != attempts.Load() {
		t.Errorf("unexpected failures within configured capacity: %v", statuses)
	}
	result, _ := json.Marshal(map[string]any{"concurrency": concurrency, "stream": stream, "accounts": accounts, "per_account": perAccount,
		"mock_delay_ms": delay.Milliseconds(), "elapsed_s": elapsed.Seconds(), "statuses": statuses,
		"success_rps": float64(len(latencies)) / elapsed.Seconds(), "p50_ms": quantile(.5), "p95_ms": quantile(.95), "p99_ms": quantile(.99),
		"peak_upstream": peak.Load(), "peak_process_heap_mib": float64(peakHeap.Load()) / (1 << 20), "persisted_requests": snapshot.Requests})
	fmt.Println("LOADTEST", string(result))
}
