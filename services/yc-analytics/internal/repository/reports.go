package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// Reports reads the aggregates. A tenantID of "" covers every tenant.
type Reports struct{}

const revenueSums = `SUM(orders)::bigint, SUM(cancelled)::bigint, SUM(items_minor)::bigint, SUM(tax_minor)::bigint,
	SUM(shipping_minor)::bigint, SUM(gross_minor)::bigint, SUM(refunded_minor)::bigint, SUM(net_minor)::bigint`

func scanRevenue(dst *domain.RevenueRow, r pgx.CollectableRow, lead ...any) error {
	return r.Scan(append(lead, &dst.Currency, &dst.Orders, &dst.Cancelled, &dst.ItemsMinor, &dst.TaxMinor,
		&dst.ShippingMinor, &dst.GrossMinor, &dst.RefundedMinor, &dst.NetMinor)...)
}

// RevenueDaily is revenue per date and currency.
func (Reports) RevenueDaily(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range) ([]domain.RevenueRow, error) {
	rows, err := tx.Query(ctx, `SELECT date_key, currency, `+revenueSums+` FROM agg_daily_revenue
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY date_key, currency ORDER BY date_key, currency`, tenantID, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("daily revenue: %w", err)
	}
	return collect(rows, "daily revenue", func(row pgx.CollectableRow) (domain.RevenueRow, error) {
		var v domain.RevenueRow
		err := scanRevenue(&v, row, &v.Date)
		return v, err
	})
}

// RevenueTotals is revenue per currency over the range, per tenant when
// byTenant is set.
func (Reports) RevenueTotals(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range, byTenant bool) ([]domain.RevenueRow, error) {
	group := "''"
	if byTenant {
		group = "tenant_id"
	}
	rows, err := tx.Query(ctx, `SELECT `+group+`, currency, `+revenueSums+` FROM agg_daily_revenue
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY 1, currency ORDER BY 1, SUM(gross_minor) DESC, currency`, tenantID, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("revenue totals: %w", err)
	}
	return collect(rows, "revenue totals", func(row pgx.CollectableRow) (domain.RevenueRow, error) {
		var v domain.RevenueRow
		err := scanRevenue(&v, row, &v.TenantID)
		return v, err
	})
}

const funnelSums = `SUM(visitors)::bigint, SUM(carts_created)::bigint, SUM(checkouts_started)::bigint,
	SUM(checkouts_confirmed)::bigint, SUM(orders_placed)::bigint`

func scanFunnel(dst *domain.FunnelRow, r pgx.CollectableRow, lead any) error {
	return r.Scan(lead, &dst.Visitors, &dst.CartsCreated, &dst.CheckoutsStarted, &dst.CheckoutsConfirmed, &dst.OrdersPlaced)
}

// FunnelDaily is the funnel per date.
func (Reports) FunnelDaily(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range) ([]domain.FunnelRow, error) {
	rows, err := tx.Query(ctx, `SELECT date_key, `+funnelSums+` FROM agg_daily_funnel
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY date_key ORDER BY date_key`, tenantID, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("daily funnel: %w", err)
	}
	return collect(rows, "daily funnel", func(row pgx.CollectableRow) (domain.FunnelRow, error) {
		var v domain.FunnelRow
		err := scanFunnel(&v, row, &v.Date)
		return v, err
	})
}

// FunnelTotals is the funnel over the range, per tenant when byTenant is
// set.
func (Reports) FunnelTotals(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range, byTenant bool) ([]domain.FunnelRow, error) {
	group := "''"
	if byTenant {
		group = "tenant_id"
	}
	rows, err := tx.Query(ctx, `SELECT `+group+`, `+funnelSums+` FROM agg_daily_funnel
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY 1 ORDER BY 1`, tenantID, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("funnel totals: %w", err)
	}
	out, err := collect(rows, "funnel totals", func(row pgx.CollectableRow) (domain.FunnelRow, error) {
		var v domain.FunnelRow
		err := scanFunnel(&v, row, &v.TenantID)
		return v, err
	})
	if err != nil || len(out) > 0 || byTenant {
		return out, err
	}
	return []domain.FunnelRow{{TenantID: tenantID}}, nil
}

// ProductSort orders top products.
type ProductSort string

// Product sorts.
const (
	ByRevenue ProductSort = "revenue"
	ByUnits   ProductSort = "units"
)

// TopProducts is the best-selling SKUs over the range.
func (Reports) TopProducts(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range, by ProductSort, limit int) ([]domain.ProductRow, error) {
	order := "SUM(revenue_minor) DESC, SUM(units) DESC"
	if by == ByUnits {
		order = "SUM(units) DESC, SUM(revenue_minor) DESC"
	}
	rows, err := tx.Query(ctx, `SELECT sku_id::text, MAX(name), currency, SUM(units)::bigint, SUM(revenue_minor)::bigint
		FROM agg_product_performance
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY sku_id, currency ORDER BY `+order+`, sku_id LIMIT $4`, tenantID, r.From, r.To, limit)
	if err != nil {
		return nil, fmt.Errorf("top products: %w", err)
	}
	return collect(rows, "top products", func(row pgx.CollectableRow) (domain.ProductRow, error) {
		var v domain.ProductRow
		err := row.Scan(&v.SKUID, &v.Name, &v.Currency, &v.Units, &v.RevenueMinor)
		return v, err
	})
}

// Gateways is each payment gateway's performance over the range.
func (Reports) Gateways(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Range) ([]domain.GatewayRow, error) {
	rows, err := tx.Query(ctx, `SELECT gateway, currency, SUM(attempts)::bigint, SUM(successes)::bigint,
			SUM(failures)::bigint, SUM(amount_minor)::bigint
		FROM agg_gateway_performance
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY gateway, currency ORDER BY SUM(attempts) DESC, gateway, currency`, tenantID, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("gateway performance: %w", err)
	}
	return collect(rows, "gateway performance", func(row pgx.CollectableRow) (domain.GatewayRow, error) {
		var v domain.GatewayRow
		err := row.Scan(&v.Gateway, &v.Currency, &v.Attempts, &v.Successes, &v.Failures, &v.AmountMinor)
		return v, err
	})
}

// TenantNames returns the shops' names.
func (Reports) TenantNames(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, name FROM dim_tenant`)
	if err != nil {
		return nil, fmt.Errorf("tenant names: %w", err)
	}
	out := map[string]string{}
	var id, name string
	_, err = pgx.ForEachRow(rows, []any{&id, &name}, func() error {
		out[id] = name
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tenant names: %w", err)
	}
	return out, nil
}

func collect[T any](rows pgx.Rows, what string, fn pgx.RowToFunc[T]) ([]T, error) {
	out, err := pgx.CollectRows(rows, fn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}
