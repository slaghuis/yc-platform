package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

const serviceName = "yc-tenant-cli"

// hostnames is a repeatable --hostname flag.
type hostnames []string

func (h *hostnames) String() string { return strings.Join(*h, ",") }

func (h *hostnames) Set(v string) error {
	*h = append(*h, strings.ToLower(strings.TrimSpace(v)))
	return nil
}

// createBody is POST /sysadmin/tenants (ADR-029).
type createBody struct {
	TenantID            string   `json:"tenant_id"`
	Name                string   `json:"name"`
	Hostnames           []string `json:"hostnames"`
	StoreCurrency       string   `json:"store_currency"`
	DefaultJurisdiction string   `json:"default_jurisdiction"`
	IsTaxInclusive      bool     `json:"is_tax_inclusive"`
	ContactEmail        string   `json:"contact_email"`
	PrimaryAdminEmail   *string  `json:"primary_admin_email,omitempty"`
}

type provisionOpts struct {
	gatewayURL string
	token      string
	body       createBody
}

func parseProvision(args []string, stderr io.Writer) (provisionOpts, error) {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		o     provisionOpts
		hosts hostnames
		email string
	)
	fs.StringVar(&o.gatewayURL, "gateway-url", envOr("YC_GATEWAY_URL", "http://admin.localhost:8070"),
		"yc-gateway base URL on a platform host (or YC_GATEWAY_URL)")
	fs.StringVar(&o.token, "token", os.Getenv("YC_TENANT_CLI_TOKEN"),
		"system admin bearer token (or YC_TENANT_CLI_TOKEN)")
	fs.StringVar(&o.body.TenantID, "tenant-id", "", "tenant slug, e.g. shop-abc (required)")
	fs.StringVar(&o.body.Name, "name", "", "shop display name (required)")
	fs.Var(&hosts, "hostname", "hostname served by yc-gateway; repeatable (at least one)")
	fs.StringVar(&o.body.StoreCurrency, "store-currency", "ZAR", "ISO 4217 currency")
	fs.StringVar(&o.body.DefaultJurisdiction, "default-jurisdiction", "ZA", "ISO 3166-1 alpha-2 country")
	fs.BoolVar(&o.body.IsTaxInclusive, "tax-inclusive", true, "prices include tax")
	fs.StringVar(&o.body.ContactEmail, "contact-email", "", "shop contact email (required; defaults to --primary-admin-email)")
	fs.StringVar(&email, "primary-admin-email", "", "initial shop admin email (optional)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	var errs []error
	if o.token == "" {
		errs = append(errs, errors.New("--token or YC_TENANT_CLI_TOKEN is required"))
	}
	if err := eventbus.ValidateTenantID(o.body.TenantID); err != nil || o.body.TenantID == eventbus.PlatformTenantID {
		errs = append(errs, fmt.Errorf("--tenant-id must be a lowercase slug such as shop-abc"))
	}
	if o.body.Name == "" {
		errs = append(errs, errors.New("--name is required"))
	}
	if len(hosts) == 0 {
		errs = append(errs, errors.New("at least one --hostname is required"))
	}
	if o.body.ContactEmail == "" {
		o.body.ContactEmail = email
	}
	if o.body.ContactEmail == "" {
		errs = append(errs, errors.New("--contact-email is required (or --primary-admin-email)"))
	}
	if err := errors.Join(errs...); err != nil {
		return o, err
	}

	o.body.Hostnames = hosts
	if email != "" {
		o.body.PrimaryAdminEmail = &email
	}
	o.gatewayURL = strings.TrimRight(o.gatewayURL, "/")
	return o, nil
}

func provision(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseProvision(args, stderr)
	if err != nil {
		return err
	}

	raw, err := json.Marshal(o.body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.gatewayURL+"/sysadmin/tenants", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "yc-tenant-cli:"+o.body.TenantID+":"+uuid.NewString())
	req.Header.Set("User-Agent", serviceName)

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /sysadmin/tenants: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	_, _ = fmt.Fprintf(stdout, "created %s (%s)\n", o.body.TenantID, strings.Join(o.body.Hostnames, ", "))
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
