// Command yc-identity runs the yc-identity service.
//
//	yc-identity                                  serve
//	yc-identity bootstrap-admin --email <email>  create the first system admin
//	yc-identity healthcheck [url]                container health check (/healthz)
//
// bootstrap-admin reads the password from YC_BOOTSTRAP_ADMIN_PASSWORD or,
// when that is unset, the first line of stdin. It is idempotent: an
// existing system admin with the email is left unchanged.
//
// See docs/services/yc-identity.md for the requirement spec.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/app"
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
		if len(os.Args) > 1 && os.Args[1] == "bootstrap-admin" {
			err = bootstrapAdmin(ctx, cfg, os.Args[2:], os.Stdin, os.Stdout)
		} else {
			build := httpkit.BuildInfo{Service: cfg.Service, Version: version, BuiltAt: builtAt}
			err = app.Run(ctx, cfg, build)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-identity:", err)
		os.Exit(1)
	}
}

func bootstrapAdmin(ctx context.Context, cfg app.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	email := fs.String("email", "", "system admin email (required)")
	name := fs.String("display-name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("bootstrap-admin: --email is required")
	}
	pw := os.Getenv("YC_BOOTSTRAP_ADMIN_PASSWORD")
	if pw == "" {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("bootstrap-admin: read password: %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if pw == "" {
		return errors.New("bootstrap-admin: set YC_BOOTSTRAP_ADMIN_PASSWORD or pass the password on stdin")
	}

	created, err := app.BootstrapAdmin(ctx, cfg, *email, pw, *name)
	if err != nil {
		return err
	}
	if created {
		_, _ = fmt.Fprintf(stdout, "system admin %s created\n", *email)
	} else {
		_, _ = fmt.Fprintf(stdout, "system admin %s already exists; unchanged\n", *email)
	}
	return nil
}
