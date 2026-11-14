package integration

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
)

// WaitForShutdown blocks until SIGINT/SIGTERM or until errc yields, then calls stop.
func WaitForShutdown(errc <-chan error, stop func() error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	return stop()
}
