package main

import (
	"flag"
	"log/slog"
	"os"

	"app/configs"
	"app/pkg"
	"app/pkg/integration"
)

func main() {
	configPath := flag.String("config", "", "path to JSON config file (defaults are used when empty)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := configs.Load(*configPath)
	if err != nil {
		slog.Error("invalid config", "err", err)
		os.Exit(1)
	}

	app, err := pkg.NewApp(cfg)
	if err != nil {
		slog.Error("failed to create app", "err", err)
		os.Exit(1)
	}

	errc := make(chan error, 1)
	go func() { errc <- app.Start() }()
	slog.Info("serving", "address", cfg.Address, "root", cfg.Root,
		"metricsAddress", cfg.Metrics.Address, "version", integration.Version)

	if err := integration.WaitForShutdown(errc, app.Stop); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}
