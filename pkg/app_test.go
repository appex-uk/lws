package pkg

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"app/configs"
)

func newTestApp(t *testing.T, mutate func(*configs.Config)) (*App, string, chan error) {
	app, base, _, done := newTestAppWithMetrics(t, mutate)
	return app, base, done
}

func newTestAppWithMetrics(t *testing.T, mutate func(*configs.Config)) (*App, string, string, chan error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := configs.Default()
	cfg.Root = dir
	cfg.Timeouts.Drain = 0
	cfg.Metrics.Address = "127.0.0.1:0"
	if mutate != nil {
		mutate(&cfg)
	}
	app, err := NewApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var metricsLn net.Listener
	metricsBase := ""
	if cfg.Metrics.Address != "" {
		if metricsLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		metricsBase = "http://" + metricsLn.Addr().String()
	}
	done := make(chan error, 1)
	go func() { done <- app.Serve(ln, metricsLn) }()
	return app, "http://" + ln.Addr().String(), metricsBase, done
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAppServesContentAndProbes(t *testing.T) {
	t.Parallel()
	app, base, done := newTestApp(t, func(c *configs.Config) { c.AccessLog = true })

	for path, want := range map[string]string{"/": "hello", "/livez": "ok", "/readyz": "ok"} {
		if code, body := get(t, base+path); code != 200 || body != want {
			t.Errorf("%s: got %d %q, want 200 %q", path, code, body, want)
		}
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve returned %v", err)
	}
}

func TestReadinessFailsWhileDraining(t *testing.T) {
	t.Parallel()
	app, base, done := newTestApp(t, func(c *configs.Config) {
		c.Timeouts.Drain = configs.Duration(300 * time.Millisecond)
	})

	stopped := make(chan error, 1)
	go func() { stopped <- app.Stop() }()
	time.Sleep(50 * time.Millisecond)

	if code, body := get(t, base+"/readyz"); code != http.StatusServiceUnavailable || body != "draining" {
		t.Errorf("readyz while draining: got %d %q", code, body)
	}
	if code, _ := get(t, base+"/livez"); code != 200 {
		t.Errorf("livez while draining: got %d", code)
	}
	if code, body := get(t, base+"/"); code != 200 || body != "hello" {
		t.Errorf("content while draining: got %d %q", code, body)
	}

	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProbesDisabled(t *testing.T) {
	t.Parallel()
	app, base, _ := newTestApp(t, func(c *configs.Config) { c.Probes = configs.Probes{} })
	t.Cleanup(func() { app.Stop() })

	if code, _ := get(t, base+"/livez"); code != http.StatusNotFound {
		t.Errorf("livez: got %d, want 404", code)
	}
	if code, _ := get(t, base+"/readyz"); code != http.StatusNotFound {
		t.Errorf("readyz: got %d, want 404", code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	app, base, metrics, done := newTestAppWithMetrics(t, nil)

	get(t, base+"/")
	get(t, base+"/missing.txt")
	get(t, base+"/livez")
	get(t, base+"/readyz")

	code, out := get(t, metrics+"/metrics")
	if code != 200 {
		t.Fatalf("metrics status %d", code)
	}
	for _, want := range []string{
		`lws_http_requests_total{method="GET",code="200"} 1`,
		`lws_http_requests_total{method="GET",code="404"} 1`,
		"lws_http_request_duration_seconds_count 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q (probes must not be counted):\n%s", want, out)
		}
	}
	if code, _ := get(t, base+"/metrics"); code != http.StatusNotFound {
		t.Errorf("metrics exposed on public port: %d", code)
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMetricsDisabled(t *testing.T) {
	t.Parallel()
	app, _, metrics, done := newTestAppWithMetrics(t, func(c *configs.Config) { c.Metrics.Address = "" })
	if metrics != "" || app.metricsServer != nil {
		t.Fatal("metrics server should be disabled")
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewAppMissingRoot(t *testing.T) {
	t.Parallel()
	cfg := configs.Default()
	cfg.Root = filepath.Join(t.TempDir(), "missing")
	if _, err := NewApp(cfg); err == nil {
		t.Fatal("expected error")
	}
}
