// Command yc-tenant runs the yc-tenant service.
//
// See docs/services/yc-tenant.md for the requirement spec.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/app"
)

// Set by the build (-ldflags "-X main.version=… -X main.builtAt=…").
var (
	version = "dev"
	builtAt = ""
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(httpkit.Probe(os.Args[2:]))
		case "import":
			os.Exit(runImport(os.Args[2:]))
		}
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
		fmt.Fprintln(os.Stderr, "yc-tenant:", err)
		os.Exit(1)
	}
}

// runImport imports existing shops from yc-gateway and yc-pricing without
// publishing provisioned.v1 (ADR-029 rule 11).
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		gatewayDB    = fs.String("gateway-db-url", os.Getenv("YC_GATEWAY_DB_URL"), "yc-gateway Postgres URL")
		pricingDB    = fs.String("pricing-db-url", os.Getenv("YC_PRICING_DB_URL"), "yc-pricing Postgres URL")
		contactEmail = fs.String("contact-email", "", "contact email applied to every imported shop (required)")
		nameTpl      = fs.String("name-template", "{tenant}", "shop name; {tenant} is replaced")
		staticHosts  = fs.String("static-hosts", "", "comma-separated hostname=tenant_id from yc-gateway config")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := app.LoadConfig()
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-tenant import:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	created, skipped, err := app.RunImport(ctx, cfg, app.ImportOpts{
		GatewayDBURL: *gatewayDB, PricingDBURL: *pricingDB,
		ContactEmail: *contactEmail, NameTemplate: *nameTpl,
		StaticHosts: parseStaticHosts(*staticHosts),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yc-tenant import:", err)
		return 1
	}
	fmt.Printf("import complete: created=%d skipped=%d\n", created, skipped)
	return 0
}

func parseStaticHosts(s string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, tenant, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		host = strings.ToLower(strings.TrimSpace(host))
		tenant = strings.TrimSpace(tenant)
		if host != "" && tenant != "" {
			out[host] = tenant
		}
	}
	return out
}
