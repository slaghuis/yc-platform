// Command yc-orders runs the yc-orders service.
//
// See docs/services/yc-orders.md for the requirement spec.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/app"
)

// Set by the build (-ldflags "-X main.version=… -X main.builtAt=…").
var (
	version = "dev"
	builtAt = ""
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := app.LoadConfig()
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil {
		err = app.Run(ctx, cfg, httpkit.BuildInfo{Service: cfg.Service, Version: version, BuiltAt: builtAt})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-orders:", err)
		os.Exit(1)
	}
}
