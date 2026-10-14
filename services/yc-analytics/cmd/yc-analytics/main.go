// Command yc-analytics runs the yc-analytics service.
//
// See docs/services/yc-analytics.md for the requirement spec.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).
		With("service", "yc-analytics", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("hello")
	<-ctx.Done()
	logger.Info("shutting down")
}
