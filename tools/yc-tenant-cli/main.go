// yc-tenant-cli provisions tenants through POST /sysadmin/tenants
// (ADR-029, docs/runbooks/onboarding-tenant.md).
//
// Phase 11 implements `provision` as an API client; suspend, delete and
// verify follow.
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
  provision   create a shop via POST /sysadmin/tenants

Requires a system admin bearer token (YC_TENANT_CLI_TOKEN) and a platform
host gateway URL (YC_GATEWAY_URL, default http://admin.localhost:8070).

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
