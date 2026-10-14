package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

const serviceName = "yc-tenant-cli"

// hostnames is a repeatable --hostname flag.
type hostnames []string

func (h *hostnames) String() string { return strings.Join(*h, ",") }

func (h *hostnames) Set(v string) error {
	*h = append(*h, strings.ToLower(strings.TrimSpace(v)))
	return nil
}

type provisionOpts struct {
	natsURL string
	payload events.PlatformTenantProvisionedV1
}

func parseProvision(args []string, stderr io.Writer, now time.Time) (provisionOpts, error) {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		o     provisionOpts
		hosts hostnames
		email string
	)
	fs.StringVar(&o.natsURL, "nats-url", envOr("NATS_URL", "nats://localhost:4222"), "NATS URL")
	fs.StringVar(&o.payload.TenantID, "tenant-id", "", "tenant slug, e.g. shop-abc (required)")
	fs.StringVar(&o.payload.Name, "name", "", "shop display name (required)")
	fs.Var(&hosts, "hostname", "hostname served by yc-gateway; repeatable (at least one)")
	fs.StringVar(&o.payload.StoreCurrency, "store-currency", "ZAR", "ISO 4217 currency")
	fs.StringVar(&o.payload.DefaultJurisdiction, "default-jurisdiction", "ZA", "ISO 3166-1 alpha-2 country")
	fs.BoolVar(&o.payload.IsTaxInclusive, "tax-inclusive", true, "prices include tax")
	fs.StringVar(&email, "primary-admin-email", "", "initial shop admin email (optional)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	var errs []error
	if err := eventbus.ValidateTenantID(o.payload.TenantID); err != nil || o.payload.TenantID == eventbus.PlatformTenantID {
		errs = append(errs, fmt.Errorf("--tenant-id must be a lowercase slug such as shop-abc"))
	}
	if o.payload.Name == "" {
		errs = append(errs, errors.New("--name is required"))
	}
	if len(hosts) == 0 {
		errs = append(errs, errors.New("at least one --hostname is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return o, err
	}

	o.payload.Hostnames = hosts
	if email != "" {
		o.payload.PrimaryAdminEmail = &email
	}
	o.payload.ProvisionedAt = now.UTC()
	return o, nil
}

func provision(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseProvision(args, stderr, time.Now())
	if err != nil {
		return err
	}

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:  serviceName,
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		LogLevel:     "warn",
	})
	if err != nil {
		return err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()

	validator, err := events.NewValidator()
	if err != nil {
		return err
	}
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL:     o.natsURL,
		ServiceName: serviceName,
		Validator:   validator,
		Streams:     []eventbus.StreamConfig{eventbus.PlatformStream()},
	})
	if err != nil {
		return err
	}
	defer func() { _ = bus.Close(context.WithoutCancel(ctx)) }()

	if err := bus.PublishDirect(ctx, eventbus.Event{
		EventType: events.TypePlatformTenantProvisionedV1,
		TenantID:  eventbus.PlatformTenantID,
		Actor:     eventbus.Actor{Type: "service", ID: serviceName},
		Payload:   o.payload,
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "published %s for %s (%s)\n",
		events.TypePlatformTenantProvisionedV1, o.payload.TenantID, strings.Join(o.payload.Hostnames, ", "))
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
