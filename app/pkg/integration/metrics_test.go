package integration

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type %q", ct)
	}
	return rec.Body.String()
}

func TestInstrumentRecordsMetrics(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	h := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/slow":
			time.Sleep(30 * time.Millisecond)
			io.WriteString(w, "slow")
		default:
			io.WriteString(w, "hello") // implicit 200
		}
	}), m, false)

	for _, req := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/"}, {"HEAD", "/"}, {"GET", "/missing"}, {"DELETE", "/"}, {"GET", "/slow"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(req.method, req.path, nil))
	}

	out := scrape(t, m)
	for _, want := range []string{
		`lws_http_requests_total{method="GET",code="200"} 3`,
		`lws_http_requests_total{method="HEAD",code="200"} 1`,
		`lws_http_requests_total{method="GET",code="404"} 1`,
		`lws_http_requests_total{method="OTHER",code="200"} 1`,
		`lws_http_requests_in_flight 0`,
		`lws_http_request_duration_seconds_bucket{le="+Inf"} 6`,
		`lws_http_request_duration_seconds_count 6`,
		`lws_http_request_duration_seconds_bucket{le="0.025"} 5`,
		`lws_http_request_duration_seconds_bucket{le="0.05"} 6`,
		`lws_build_info{version="dev",go_version="go`,
		"go_goroutines ",
		"process_start_time_seconds ",
		"process_cpu_seconds_total ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// 5+5+0(HEAD)+19("404 page not found\n")+5+4
	if !strings.Contains(out, "lws_http_response_bytes_total 38\n") {
		t.Errorf("unexpected byte count in:\n%s", out)
	}
}

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[a-zA-Z_][a-zA-Z0-9_]*="[^"]*"(,[a-zA-Z_][a-zA-Z0-9_]*="[^"]*")*\})? -?[0-9.eE+-]+$`)
	typeLine   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (counter|gauge|histogram)$`)
)

// TestExpositionFormat checks every line is valid and every sample belongs
// to a family declared with # TYPE, with no family declared twice.
func TestExpositionFormat(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	h := Instrument(http.NotFoundHandler(), m, false)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	declared := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(scrape(t, m)))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		if mt := typeLine.FindStringSubmatch(line); mt != nil {
			if _, dup := declared[mt[1]]; dup {
				t.Errorf("duplicate TYPE for %s", mt[1])
			}
			declared[mt[1]] = mt[2]
			continue
		}
		mt := sampleLine.FindStringSubmatch(line)
		if mt == nil {
			t.Errorf("invalid line %q", line)
			continue
		}
		name := mt[1]
		family := name
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suf); ok && declared[base] == "histogram" {
				family = base
			}
		}
		if _, ok := declared[family]; !ok {
			t.Errorf("sample %q has no TYPE", name)
		}
	}
}

func TestInstrumentDisabledReturnsNext(t *testing.T) {
	t.Parallel()
	next := http.NotFoundHandler()
	if got := Instrument(next, nil, false); got == nil {
		t.Fatal("nil handler")
	}
}

// TestInstrumentKeepsReaderFrom ensures wrapped responses can still use
// io.ReaderFrom (sendfile on a real connection).
func TestInstrumentKeepsReaderFrom(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewMetrics()
	var sawReaderFrom bool
	h := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawReaderFrom = w.(io.ReaderFrom)
		http.ServeFile(w, r, p)
	}), m, true)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "0123456789" || !sawReaderFrom {
		t.Fatalf("body %q readerFrom %v", body, sawReaderFrom)
	}
	if out := scrape(t, m); !strings.Contains(out, "lws_http_response_bytes_total 10\n") {
		t.Errorf("bytes not counted:\n%s", out)
	}
}

func BenchmarkInstrument(b *testing.B) {
	m := NewMetrics()
	h := Instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}), m, false)
	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := discardWriter{header: http.Header{}}
		for pb.Next() {
			h.ServeHTTP(w, req)
		}
	})
}

type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}
