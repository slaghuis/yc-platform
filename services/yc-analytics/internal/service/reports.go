package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/repository"
)

// Top-product limits.
const (
	DefaultTopProducts = 10
	MaxTopProducts     = 100
	dashboardProducts  = 5
)

// Range checks a report's dates; unset ends default to the 30 days
// ending today.
func (s *Service) Range(from, to *time.Time) (domain.Range, error) {
	r, err := domain.NewRange(from, to, s.now())
	if err != nil {
		return r, ycerrors.Validation().Field("from", "invalid_range", err.Error()).Build()
	}
	return r, nil
}

// Dashboard summarises a shop (shop admin) or every shop (system admin)
// over a range.
type Dashboard struct {
	Range       domain.Range
	Revenue     []domain.RevenueRow
	Funnel      domain.FunnelRow
	TopProducts []domain.ProductRow
	Gateways    []domain.GatewayRow
	// PendingDays have fact changes not yet in the aggregates.
	PendingDays int64
}

// Dashboard returns the caller's dashboard.
func (s *Service) Dashboard(ctx context.Context, r domain.Range) (Dashboard, error) {
	d := Dashboard{Range: r}
	err := s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if d.Revenue, err = s.reports.RevenueTotals(ctx, tx, tenantID, r, false); err != nil {
			return err
		}
		funnel, err := s.reports.FunnelTotals(ctx, tx, tenantID, r, false)
		if err != nil {
			return err
		}
		d.Funnel = funnel[0]
		if d.TopProducts, err = s.reports.TopProducts(ctx, tx, tenantID, r, repository.ByRevenue, dashboardProducts); err != nil {
			return err
		}
		if d.Gateways, err = s.reports.Gateways(ctx, tx, tenantID, r); err != nil {
			return err
		}
		d.PendingDays, err = s.aggs.Pending(ctx, tx, tenantID)
		return err
	})
	return d, err
}

// Revenue returns daily revenue per currency.
func (s *Service) Revenue(ctx context.Context, r domain.Range) ([]domain.RevenueRow, error) {
	var out []domain.RevenueRow
	err := s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.reports.RevenueDaily(ctx, tx, tenantID, r)
		return err
	})
	return out, err
}

// TopProducts returns the best-selling products.
func (s *Service) TopProducts(ctx context.Context, r domain.Range, by string, limit int) ([]domain.ProductRow, error) {
	sort, limit, err := productQuery(by, limit)
	if err != nil {
		return nil, err
	}
	var out []domain.ProductRow
	err = s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.reports.TopProducts(ctx, tx, tenantID, r, sort, limit)
		return err
	})
	return out, err
}

func productQuery(by string, limit int) (repository.ProductSort, int, error) {
	v := ycerrors.Validation()
	sort := repository.ProductSort(by)
	switch sort {
	case "":
		sort = repository.ByRevenue
	case repository.ByRevenue, repository.ByUnits:
	default:
		v.Field("by", "invalid", "by must be revenue or units")
	}
	if limit == 0 {
		limit = DefaultTopProducts
	}
	if limit < 1 || limit > MaxTopProducts {
		v.Field("limit", "out_of_range", fmt.Sprintf("limit must be between 1 and %d", MaxTopProducts))
	}
	if v.HasErrors() {
		return "", 0, v.Build()
	}
	return sort, limit, nil
}

// Funnel returns the funnel's totals and daily counts.
func (s *Service) Funnel(ctx context.Context, r domain.Range) (domain.FunnelRow, []domain.FunnelRow, error) {
	var total domain.FunnelRow
	var daily []domain.FunnelRow
	err := s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		totals, err := s.reports.FunnelTotals(ctx, tx, tenantID, r, false)
		if err != nil {
			return err
		}
		total = totals[0]
		daily, err = s.reports.FunnelDaily(ctx, tx, tenantID, r)
		return err
	})
	return total, daily, err
}

// Gateways returns payment gateway performance.
func (s *Service) Gateways(ctx context.Context, r domain.Range) ([]domain.GatewayRow, error) {
	var out []domain.GatewayRow
	err := s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.reports.Gateways(ctx, tx, tenantID, r)
		return err
	})
	return out, err
}

// TenantComparison is one shop's figures in the comparison.
type TenantComparison struct {
	TenantID string
	Name     string
	Revenue  []domain.RevenueRow
	Funnel   domain.FunnelRow
}

// CompareTenants returns every shop's revenue and funnel over r, highest
// gross revenue first within each currency (system admin).
func (s *Service) CompareTenants(ctx context.Context, r domain.Range) ([]TenantComparison, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	var out []TenantComparison
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		names, err := s.reports.TenantNames(ctx, tx)
		if err != nil {
			return err
		}
		revenue, err := s.reports.RevenueTotals(ctx, tx, "", r, true)
		if err != nil {
			return err
		}
		funnels, err := s.reports.FunnelTotals(ctx, tx, "", r, true)
		if err != nil {
			return err
		}
		index := map[string]int{}
		row := func(tenantID string) *TenantComparison {
			i, ok := index[tenantID]
			if !ok {
				i = len(out)
				index[tenantID] = i
				out = append(out, TenantComparison{
					TenantID: tenantID, Name: names[tenantID],
					Funnel: domain.FunnelRow{TenantID: tenantID},
				})
			}
			return &out[i]
		}
		for _, rv := range revenue {
			c := row(rv.TenantID)
			c.Revenue = append(c.Revenue, rv)
		}
		for _, f := range funnels {
			row(f.TenantID).Funnel = f
		}
		return nil
	})
	return out, err
}

// Export renders a report as CSV and returns its file name.
func (s *Service) Export(ctx context.Context, query string, r domain.Range) (string, []byte, error) {
	if !domain.ValidQuery(query) {
		return "", nil, ycerrors.NotFound("Export", query)
	}
	var records [][]string
	err := s.read(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		records, err = s.exportRecords(ctx, tx, tenantID, query, r)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(records); err != nil {
		return "", nil, fmt.Errorf("write csv: %w", err)
	}
	name := fmt.Sprintf("%s_%s_%s.csv", query, r.From.Format(domain.DateLayout), r.To.Format(domain.DateLayout))
	return name, buf.Bytes(), nil
}

func (s *Service) exportRecords(ctx context.Context, tx pgx.Tx, tenantID, query string, r domain.Range) ([][]string, error) {
	switch query {
	case domain.QueryRevenue:
		rows, err := s.reports.RevenueDaily(ctx, tx, tenantID, r)
		return table([]string{
			"date", "currency", "orders", "cancelled", "items_minor", "tax_minor",
			"shipping_minor", "gross_minor", "refunded_minor", "net_minor",
		}, rows, func(v domain.RevenueRow) []string {
			return append([]string{v.Date.Format(domain.DateLayout), v.Currency}, ints(v.Orders, v.Cancelled,
				v.ItemsMinor, v.TaxMinor, v.ShippingMinor, v.GrossMinor, v.RefundedMinor, v.NetMinor)...)
		}), err
	case domain.QueryTopProducts:
		rows, err := s.reports.TopProducts(ctx, tx, tenantID, r, repository.ByRevenue, MaxTopProducts)
		return table([]string{"sku_id", "name", "currency", "units", "revenue_minor"}, rows,
			func(v domain.ProductRow) []string {
				return append([]string{v.SKUID, v.Name, v.Currency}, ints(v.Units, v.RevenueMinor)...)
			}), err
	case domain.QueryFunnel:
		rows, err := s.reports.FunnelDaily(ctx, tx, tenantID, r)
		return table([]string{"date", "visitors", "carts_created", "checkouts_started", "checkouts_confirmed", "orders_placed"},
			rows, func(v domain.FunnelRow) []string {
				return append([]string{v.Date.Format(domain.DateLayout)}, ints(v.Visitors, v.CartsCreated,
					v.CheckoutsStarted, v.CheckoutsConfirmed, v.OrdersPlaced)...)
			}), err
	case domain.QueryGateways:
		rows, err := s.reports.Gateways(ctx, tx, tenantID, r)
		return table([]string{"gateway", "currency", "attempts", "successes", "failures", "amount_minor"}, rows,
			func(v domain.GatewayRow) []string {
				return append([]string{v.Gateway, v.Currency}, ints(v.Attempts, v.Successes, v.Failures, v.AmountMinor)...)
			}), err
	}
	return nil, ycerrors.NotFound("Export", query)
}

// table is a header row followed by a row per item.
func table[T any](header []string, rows []T, fn func(T) []string) [][]string {
	out := make([][]string, 0, len(rows)+1)
	out = append(out, header)
	for _, v := range rows {
		out = append(out, fn(v))
	}
	return out
}

func ints(ns ...int64) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.FormatInt(n, 10)
	}
	return out
}
