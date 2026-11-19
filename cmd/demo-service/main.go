// Command demo-service is a zero-dependency HTTP service that exposes
// Prometheus-format metrics. It gives the rest of the stack
// (Prometheus, Alertmanager, Grafana, webhook receiver) something real
// to scrape, alert on, and remediate.
//
// The exposition endpoint is hand-rolled to keep go.mod dependency-free;
// production services should use github.com/prometheus/client_golang instead.
package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Histogram buckets mirror prometheus.DefBuckets (seconds).
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type metrics struct {
	mu       sync.Mutex
	inFlight int64
	requests map[string]int64   // "handler|code" -> count
	hist     map[string][]int64 // handler -> non-cumulative per-bucket counts (last slot = +Inf)
	sum      map[string]float64 // handler -> total seconds
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[string]int64{},
		hist:     map[string][]int64{},
		sum:      map[string]float64{},
	}
}

func (m *metrics) observe(handler, code string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[handler+"|"+code]++
	b := m.hist[handler]
	if b == nil {
		b = make([]int64, len(buckets)+1)
		m.hist[handler] = b
	}
	s := d.Seconds()
	// SearchFloat64s returns the first bucket >= s, or len(buckets) -> +Inf slot.
	b[sort.SearchFloat64s(buckets, s)]++
	m.sum[handler] += s
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func instrument(m *metrics, name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.mu.Lock()
		m.inFlight++
		m.mu.Unlock()
		rw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next(rw, r)
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
		m.observe(name, strconv.Itoa(rw.code), time.Since(start))
	}
}

// render writes the Prometheus text exposition format (version 0.0.4).
func (m *metrics) render(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder

	b.WriteString("# HELP demo_http_requests_total Total HTTP requests by handler and status code.\n# TYPE demo_http_requests_total counter\n")
	rkeys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		rkeys = append(rkeys, k)
	}
	sort.Strings(rkeys)
	for _, k := range rkeys {
		i := strings.LastIndex(k, "|")
		fmt.Fprintf(&b, `demo_http_requests_total{handler="%s",code="%s"} %d`+"\n", k[:i], k[i+1:], m.requests[k])
	}

	b.WriteString("# HELP demo_http_requests_in_flight HTTP requests currently being served.\n# TYPE demo_http_requests_in_flight gauge\n")
	fmt.Fprintf(&b, "demo_http_requests_in_flight %d\n", m.inFlight)

	b.WriteString("# HELP demo_http_request_duration_seconds HTTP request latency in seconds.\n# TYPE demo_http_request_duration_seconds histogram\n")
	hkeys := make([]string, 0, len(m.hist))
	for h := range m.hist {
		hkeys = append(hkeys, h)
	}
	sort.Strings(hkeys)
	for _, h := range hkeys {
		var cum int64
		for i, le := range buckets {
			cum += m.hist[h][i]
			fmt.Fprintf(&b, `demo_http_request_duration_seconds_bucket{handler="%s",le="%s"} %d`+"\n", h, strconv.FormatFloat(le, 'g', -1, 64), cum)
		}
		cum += m.hist[h][len(buckets)]
		fmt.Fprintf(&b, `demo_http_request_duration_seconds_bucket{handler="%s",le="+Inf"} %d`+"\n", h, cum)
		fmt.Fprintf(&b, `demo_http_request_duration_seconds_sum{handler="%s"} %s`+"\n", h, strconv.FormatFloat(m.sum[h], 'g', -1, 64))
		fmt.Fprintf(&b, `demo_http_request_duration_seconds_count{handler="%s"} %d`+"\n", h, cum)
	}

	fmt.Fprint(w, b.String())
}

// fetchOrderDetail simulates loading one related detail row via an indexed
// lookup (~12-20ms each).
func fetchOrderDetail(_ int) {
	time.Sleep(time.Duration(12+rand.Intn(8)) * time.Millisecond)
}

// workHandler simulates real work. ?mode=slow forces a tail-latency request,
// ?mode=error produces 5xx; default traffic is fast with a ~3% natural tail
// so the P99 alert fires on its own once enough traffic accumulates.
func workHandler(w http.ResponseWriter, r *http.Request) {
	var d time.Duration
	code := http.StatusOK
	switch r.URL.Query().Get("mode") {
	case "slow":
		d = time.Duration(500+rand.Intn(400)) * time.Millisecond
	case "error":
		d = time.Duration(10+rand.Intn(50)) * time.Millisecond
		if rand.Intn(100) < 40 {
			code = http.StatusInternalServerError
		}
	default:
		// 每条工单带出关联明细行
		for i := 0; i < 30; i++ {
			fetchOrderDetail(i)
		}
		d = time.Duration(5+rand.Intn(45)) * time.Millisecond
		if rand.Intn(100) < 3 {
			d = time.Duration(550+rand.Intn(300)) * time.Millisecond
		}
	}
	time.Sleep(d)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"ok":%t,"latency_ms":%d}`, code == http.StatusOK, d.Milliseconds())
}

// loadgen keeps a trickle of self-traffic flowing so dashboards and alert
// rules have live data the moment the stack comes up.
func loadgen(port string) {
	client := &http.Client{Timeout: 3 * time.Second}
	time.Sleep(2 * time.Second)
	for {
		mode := "fast"
		roll := rand.Intn(100)
		if roll < 4 {
			mode = "slow"
		} else if roll < 6 {
			mode = "error"
		}
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/work?mode=%s", port, mode)); err == nil {
			resp.Body.Close()
		}
		time.Sleep(time.Duration(100+rand.Intn(200)) * time.Millisecond)
	}
}

func main() {
	m := newMetrics()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.render)
	mux.HandleFunc("/healthz", instrument(m, "/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	mux.HandleFunc("/work", instrument(m, "/work", workHandler))
	if os.Getenv("LOADGEN") == "true" {
		go loadgen(port)
	}
	log.Printf("demo-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
