// yc-tenant-cli provisions tenants by publishing platform tenant events
// (docs/runbooks/onboarding-tenant.md).
//
// Phase 1 implements `provision`; suspend, delete and verify follow.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: yc-tenant-cli <command> [flags]

commands:
  provision   publish platform.tenant.provisioned.v1

Run 'yc-tenant-cli <command> -h' for the command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "yc-tenant-cli:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("missing command")
	}
	switch args[0] {
	case "provision":
		return provision(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return nil
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
