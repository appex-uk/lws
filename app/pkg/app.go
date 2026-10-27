package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"app/configs"
	"app/pkg/business/static"
	"app/pkg/integration"
)

type App struct {
	cfg           configs.Config
	root          *os.Root
	server        *http.Server
	metricsServer *http.Server
	draining      atomic.Bool
}

func NewApp(cfg configs.Config) (*App, error) {
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("open content root: %w", err)
	}

	a := &App{cfg: cfg, root: root}

	mux := http.NewServeMux()
	if p := cfg.Probes.LivenessPath; p != "" {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			probeResponse(w, http.StatusOK, "ok")
		})
	}
	if p := cfg.Probes.ReadinessPath; p != "" {
		mux.HandleFunc("GET "+p, a.readiness)
	}
	rules := make([]static.HeaderRule, len(cfg.HeaderRules))
	for i, r := range cfg.HeaderRules {
		rules[i] = static.HeaderRule{Match: r.Match, Headers: r.Headers}
	}
	files, err := static.NewHandler(root, static.Options{
		Headers:       cfg.Headers,
		Rules:         rules,
		Fallback:      cfg.Fallback,
		NotFoundPage:  cfg.NotFoundPage,
		Precompressed: cfg.Precompressed,
	})
	if err != nil {
		root.Close()
		return nil, err
	}

	// Only file requests are instrumented, so probes don't skew metrics or logs.
	var m *integration.Metrics
	if cfg.Metrics.Address != "" {
		m = integration.NewMetrics()
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET "+cfg.Metrics.Path, m)
		a.metricsServer = &http.Server{
			Addr:              cfg.Metrics.Address,
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      10 * time.Second,
		}
	}
	mux.Handle("/", integration.Instrument(files, m, cfg.AccessLog))

	t := cfg.Timeouts
	a.server = &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: time.Duration(t.ReadHeader),
		ReadTimeout:       time.Duration(t.Read),
		WriteTimeout:      time.Duration(t.Write),
		IdleTimeout:       time.Duration(t.Idle),
	}
	return a, nil
}

// readiness fails while draining or when the content root is unreadable
// (e.g. a volume that is not mounted yet or has gone away).
func (a *App) readiness(w http.ResponseWriter, _ *http.Request) {
	if a.draining.Load() {
		probeResponse(w, http.StatusServiceUnavailable, "draining")
		return
	}
	if _, err := a.root.Stat("."); err != nil {
		probeResponse(w, http.StatusServiceUnavailable, "content root unavailable")
		return
	}
	probeResponse(w, http.StatusOK, "ok")
}

func probeResponse(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// Handler exposes the root handler (useful for tests).
func (a *App) Handler() http.Handler { return a.server.Handler }

// Start listens on the configured addresses and serves until Stop is called.
func (a *App) Start() error {
	ln, err := net.Listen("tcp", a.cfg.Address)
	if err != nil {
		return err
	}
	var metricsLn net.Listener
	if a.metricsServer != nil {
		if metricsLn, err = net.Listen("tcp", a.cfg.Metrics.Address); err != nil {
			ln.Close()
			return fmt.Errorf("metrics listener: %w", err)
		}
	}
	return a.Serve(ln, metricsLn)
}

// Serve serves content on ln and metrics on metricsLn (ignored when metrics
// are disabled). It returns the first server error, or nil after Stop.
func (a *App) Serve(ln, metricsLn net.Listener) error {
	errc := make(chan error, 2)
	n := 1
	go func() { errc <- serve(a.server, ln) }()
	if a.metricsServer != nil && metricsLn != nil {
		n++
		go func() { errc <- serve(a.metricsServer, metricsLn) }()
	}
	for range n {
		if err := <-errc; err != nil {
			return err
		}
	}
	return nil
}

func serve(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop marks the app as not ready, waits for the drain period so load
// balancers stop routing to it, then gracefully shuts the server down.
func (a *App) Stop() error {
	a.draining.Store(true)
	if d := time.Duration(a.cfg.Timeouts.Drain); d > 0 {
		slog.Info("draining", "durationMs", d.Milliseconds())
		time.Sleep(d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.cfg.Timeouts.Shutdown))
	defer cancel()
	// Metrics stay up until content traffic has finished, so the last scrape
	// sees the final counts.
	errs := []error{a.server.Shutdown(ctx)}
	if a.metricsServer != nil {
		errs = append(errs, a.metricsServer.Shutdown(ctx))
	}
	return errors.Join(append(errs, a.root.Close())...)
}
