package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Version is set at build time with -ldflags "-X app/pkg/integration.Version=...".
var Version = "dev"

// durationBuckets are the upper bounds (seconds) of the request latency histogram.
var durationBuckets = [...]float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const (
	methodGet = iota
	methodHead
	methodOther
	numMethods
)

var methodNames = [numMethods]string{"GET", "HEAD", "OTHER"}

// Metrics holds lock-free request counters and renders them, together with
// Go runtime and process metrics, in the Prometheus text exposition format.
type Metrics struct {
	start    time.Time
	inFlight atomic.Int64
	// requests is indexed by method and status code (100-999).
	requests [numMethods][900]atomic.Uint64
	bytes    atomic.Uint64
	// buckets holds non-cumulative counts; the +Inf bucket is the last entry.
	buckets  [len(durationBuckets) + 1]atomic.Uint64
	durCount atomic.Uint64
	durSumNs atomic.Uint64
}

func NewMetrics() *Metrics {
	return &Metrics{start: time.Now()}
}

func methodIndex(m string) int {
	switch m {
	case http.MethodGet:
		return methodGet
	case http.MethodHead:
		return methodHead
	}
	return methodOther
}

func (m *Metrics) observe(method string, status int, n int64, d time.Duration) {
	if status >= 100 && status <= 999 {
		m.requests[methodIndex(method)][status-100].Add(1)
	}
	// Bodies written for HEAD requests are discarded by net/http.
	if method != http.MethodHead {
		m.bytes.Add(uint64(n))
	}
	secs := d.Seconds()
	i := 0
	for i < len(durationBuckets) && secs > durationBuckets[i] {
		i++
	}
	m.buckets[i].Add(1)
	m.durCount.Add(1)
	m.durSumNs.Add(uint64(d))
}

// Instrument records metrics (when m is non-nil) and writes an access log
// line per request (when accessLog is true). It returns next unchanged when
// both are disabled.
func Instrument(next http.Handler, m *Metrics, accessLog bool) http.Handler {
	if m == nil && !accessLog {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if m != nil {
			m.inFlight.Add(1)
			defer m.inFlight.Add(-1)
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		d := time.Since(start)
		if m != nil {
			m.observe(r.Method, rec.status, rec.bytes, d)
		}
		if accessLog {
			slogRequest(r, rec, d)
		}
	})
}

// ServeHTTP renders all metrics in the Prometheus text format.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	var b bytes.Buffer
	b.Grow(4096)

	header(&b, "lws_build_info", "gauge", "Build information.")
	fmt.Fprintf(&b, "lws_build_info{version=%q,go_version=%q} 1\n", Version, runtime.Version())

	header(&b, "lws_http_requests_total", "counter", "Static file requests by method and status code.")
	for mi := range m.requests {
		for ci := range m.requests[mi] {
			if v := m.requests[mi][ci].Load(); v > 0 {
				fmt.Fprintf(&b, "lws_http_requests_total{method=%q,code=\"%d\"} %d\n", methodNames[mi], ci+100, v)
			}
		}
	}

	header(&b, "lws_http_requests_in_flight", "gauge", "Static file requests currently being served.")
	fmt.Fprintf(&b, "lws_http_requests_in_flight %d\n", m.inFlight.Load())

	header(&b, "lws_http_response_bytes_total", "counter", "Response body bytes sent for static file requests.")
	fmt.Fprintf(&b, "lws_http_response_bytes_total %d\n", m.bytes.Load())

	header(&b, "lws_http_request_duration_seconds", "histogram", "Static file request latency.")
	var cum uint64
	for i, le := range durationBuckets {
		cum += m.buckets[i].Load()
		fmt.Fprintf(&b, "lws_http_request_duration_seconds_bucket{le=\"%s\"} %d\n", formatFloat(le), cum)
	}
	cum += m.buckets[len(durationBuckets)].Load()
	// _count matches the +Inf bucket so the histogram is always self-consistent.
	fmt.Fprintf(&b, "lws_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(&b, "lws_http_request_duration_seconds_sum %s\n", formatFloat(time.Duration(m.durSumNs.Load()).Seconds()))
	fmt.Fprintf(&b, "lws_http_request_duration_seconds_count %d\n", cum)

	writeRuntimeMetrics(&b)
	writeProcessMetrics(&b, m.start)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

func header(b *bytes.Buffer, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

var runtimeMetrics = []struct {
	key, name, typ, help string
}{
	{"/sched/goroutines:goroutines", "go_goroutines", "gauge", "Number of goroutines."},
	{"/sched/gomaxprocs:threads", "go_gomaxprocs", "gauge", "Value of GOMAXPROCS."},
	{"/gc/gomemlimit:bytes", "go_memlimit_bytes", "gauge", "Value of GOMEMLIMIT."},
	{"/memory/classes/total:bytes", "go_memory_mapped_bytes", "gauge", "Memory mapped by the Go runtime."},
	{"/memory/classes/heap/objects:bytes", "go_heap_objects_bytes", "gauge", "Memory occupied by live and unswept heap objects."},
	{"/gc/cycles/total:gc-cycles", "go_gc_cycles_total", "counter", "Completed GC cycles."},
}

func writeRuntimeMetrics(b *bytes.Buffer) {
	samples := make([]metrics.Sample, len(runtimeMetrics))
	for i, rm := range runtimeMetrics {
		samples[i].Name = rm.key
	}
	metrics.Read(samples)
	for i, rm := range runtimeMetrics {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			continue
		}
		header(b, rm.name, rm.typ, rm.help)
		fmt.Fprintf(b, "%s %d\n", rm.name, samples[i].Value.Uint64())
	}
}

func writeProcessMetrics(b *bytes.Buffer, start time.Time) {
	header(b, "process_start_time_seconds", "gauge", "Start time of the process since the Unix epoch.")
	fmt.Fprintf(b, "process_start_time_seconds %s\n", formatFloat(float64(start.UnixNano())/1e9))

	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		header(b, "process_cpu_seconds_total", "counter", "Total user and system CPU time.")
		fmt.Fprintf(b, "process_cpu_seconds_total %s\n", formatFloat(cpu.Seconds()))
	}

	// Linux only; skipped elsewhere.
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		header(b, "process_open_fds", "gauge", "Number of open file descriptors.")
		fmt.Fprintf(b, "process_open_fds %d\n", len(entries))
	}
	if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(statm)); len(f) > 1 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				header(b, "process_resident_memory_bytes", "gauge", "Resident memory size.")
				fmt.Fprintf(b, "process_resident_memory_bytes %d\n", pages*uint64(os.Getpagesize()))
			}
		}
	}
}
