// Command yc-message runs the yc-message service.
//
// See docs/services/yc-message.md for the requirement spec.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/app"
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
	if err == nil {
		err = app.Run(ctx, cfg, httpkit.BuildInfo{Service: cfg.Service, Version: version, BuiltAt: builtAt})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-message:", err)
		os.Exit(1)
	}
}
