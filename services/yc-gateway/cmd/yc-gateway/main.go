// Command yc-gateway runs the yc-gateway service.
//
// See docs/services/yc-gateway.md for the requirement spec.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/app"
)

// Set by the build (-ldflags "-X main.version=… -X main.builtAt=…").
var (
	version = "dev"
	builtAt = ""
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(httpkit.Probe(os.Args[2:]))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := app.LoadConfig()
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-gateway:", err)
		os.Exit(1)
	}
	build := httpkit.BuildInfo{Service: cfg.Service, Version: version, BuiltAt: builtAt}
	if err := app.Run(ctx, cfg, build); err != nil {
		fmt.Fprintln(os.Stderr, "yc-gateway:", err)
		os.Exit(1)
	}
}
