package app

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/service"
)

// ImportOpts are the databases and defaults for `yc-tenant import`.
type ImportOpts struct {
	GatewayDBURL string
	PricingDBURL string
	// StaticHosts are hostname → tenant_id from yc-gateway config, merged
	// with tenant_hosts rows.
	StaticHosts  map[string]string
	ContactEmail string
	// NameTemplate may contain "{tenant}"; default is the tenant ID.
	NameTemplate string
}

// RunImport reconstructs tenants from yc-gateway hostnames and yc-pricing
// configs and inserts them without publishing provisioned.v1 (ADR-029 rule 11).
func RunImport(ctx context.Context, cfg Config, opts ImportOpts) (created, skipped int, err error) {
	if opts.ContactEmail == "" {
		return 0, 0, fmt.Errorf("contact email is required")
	}
	if opts.GatewayDBURL == "" {
		return 0, 0, fmt.Errorf("gateway database URL is required")
	}
	if opts.PricingDBURL == "" {
		return 0, 0, fmt.Errorf("pricing database URL is required")
	}
	if opts.NameTemplate == "" {
		opts.NameTemplate = "{tenant}"
	}

	a, err := New(ctx, cfg, httpkit.BuildInfo{Service: cfg.Service, Version: "import"})
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = a.Close(context.WithoutCancel(ctx)) }()

	hosts, err := loadGatewayHosts(ctx, opts.GatewayDBURL, opts.StaticHosts)
	if err != nil {
		return 0, 0, err
	}
	pricing, err := loadPricing(ctx, opts.PricingDBURL)
	if err != nil {
		return 0, 0, err
	}

	byTenant := map[string][]string{}
	for host, tenantID := range hosts {
		byTenant[tenantID] = append(byTenant[tenantID], host)
	}
	records := make([]service.ImportRecord, 0, len(byTenant))
	for _, tenantID := range slices.Sorted(maps.Keys(byTenant)) {
		hs := byTenant[tenantID]
		slices.Sort(hs)
		p, ok := pricing[tenantID]
		if !ok {
			return 0, 0, fmt.Errorf("tenant %s has hostnames but no yc-pricing config", tenantID)
		}
		records = append(records, service.ImportRecord{
			TenantID: tenantID, Name: strings.ReplaceAll(opts.NameTemplate, "{tenant}", tenantID),
			Hostnames: hs, StoreCurrency: p.Currency, DefaultJurisdiction: p.Jurisdiction,
			IsTaxInclusive: p.TaxInclusive, ContactEmail: opts.ContactEmail,
		})
	}
	return a.Service.Import(ctx, records)
}

type pricingRow struct {
	Currency     string
	Jurisdiction string
	TaxInclusive bool
}

func loadGatewayHosts(ctx context.Context, dbURL string, static map[string]string) (map[string]string, error) {
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("gateway db: %w", err)
	}
	defer db.Close()
	out := maps.Clone(static)
	if out == nil {
		out = map[string]string{}
	}
	rows, err := db.Query(ctx, `SELECT hostname, tenant_id FROM tenant_hosts`)
	if err != nil {
		return nil, fmt.Errorf("list tenant_hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var host, tenant string
		if err := rows.Scan(&host, &tenant); err != nil {
			return nil, err
		}
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		out[host] = tenant
	}
	return out, rows.Err()
}

func loadPricing(ctx context.Context, dbURL string) (map[string]pricingRow, error) {
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("pricing db: %w", err)
	}
	defer db.Close()
	out := map[string]pricingRow{}
	err = dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, store_currency, default_jurisdiction, tax_inclusive
			FROM tenant_pricing_config`)
		if err != nil {
			return fmt.Errorf("list pricing configs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var r pricingRow
			if err := rows.Scan(&id, &r.Currency, &r.Jurisdiction, &r.TaxInclusive); err != nil {
				return err
			}
			out[id] = r
		}
		return rows.Err()
	})
	return out, err
}
