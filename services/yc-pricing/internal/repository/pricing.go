// Package repository is yc-pricing's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the shop tables.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

// Pricing reads and writes the pricing tables.
type Pricing struct{}

const configColumns = `tenant_id, store_currency, tax_inclusive, default_jurisdiction, updated_at`

// Config returns tenantID's configuration, locked when forUpdate.
func (Pricing) Config(ctx context.Context, tx pgx.Tx, tenantID string, forUpdate bool) (domain.Config, error) {
	q := `SELECT ` + configColumns + ` FROM tenant_pricing_config WHERE tenant_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var c domain.Config
	err := tx.QueryRow(ctx, q, tenantID).Scan(&c.TenantID, &c.StoreCurrency, &c.TaxInclusive, &c.DefaultJurisdiction, &c.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return domain.Config{}, ycerrors.NotFound("Pricing config", tenantID)
	}
	if err != nil {
		return domain.Config{}, fmt.Errorf("get pricing config: %w", err)
	}
	return c, nil
}

// PutConfig creates or replaces tenantID's configuration.
func (Pricing) PutConfig(ctx context.Context, tx pgx.Tx, tenantID string, in domain.ConfigInput, now time.Time) (domain.Config, error) {
	var c domain.Config
	err := tx.QueryRow(ctx, `
		INSERT INTO tenant_pricing_config (tenant_id, store_currency, tax_inclusive, default_jurisdiction, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET store_currency = EXCLUDED.store_currency,
			tax_inclusive = EXCLUDED.tax_inclusive, default_jurisdiction = EXCLUDED.default_jurisdiction,
			updated_at = EXCLUDED.updated_at
		RETURNING `+configColumns, tenantID, in.StoreCurrency, in.TaxInclusive, in.DefaultJurisdiction, now).
		Scan(&c.TenantID, &c.StoreCurrency, &c.TaxInclusive, &c.DefaultJurisdiction, &c.UpdatedAt)
	if err != nil {
		return domain.Config{}, fmt.Errorf("put pricing config: %w", err)
	}
	return c, nil
}

// InitConfig creates tenantID's configuration unless it exists, reporting
// whether it did.
func (Pricing) InitConfig(ctx context.Context, tx pgx.Tx, tenantID string, in domain.ConfigInput, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO tenant_pricing_config (tenant_id, store_currency, tax_inclusive, default_jurisdiction, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5) ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID, in.StoreCurrency, in.TaxInclusive, in.DefaultJurisdiction, now)
	if err != nil {
		return false, fmt.Errorf("init pricing config: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// StoreCurrencies lists the store currencies in use (platform transaction).
func (Pricing) StoreCurrencies(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT store_currency FROM tenant_pricing_config ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("store currencies: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store currencies: %w", err)
	}
	return out, nil
}

// CountPrices counts current base prices.
func (Pricing) CountPrices(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM base_prices WHERE valid_to IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count prices: %w", err)
	}
	return n, nil
}

const priceColumns = `sku_id, amount_minor, currency, tax_inclusive, valid_from`

func scanPrice(row pgx.Row) (domain.BasePrice, error) {
	var p domain.BasePrice
	err := row.Scan(&p.SKUID, &p.Price.AmountMinor, &p.Price.Currency, &p.TaxInclusive, &p.ValidFrom)
	return p, err
}

// CurrentPrice returns the SKU's current base price, or nil, locked for
// update.
func (Pricing) CurrentPrice(ctx context.Context, tx pgx.Tx, skuID string) (*domain.BasePrice, error) {
	p, err := scanPrice(tx.QueryRow(ctx, `SELECT `+priceColumns+` FROM base_prices
		WHERE sku_id = $1 AND valid_to IS NULL FOR UPDATE`, skuID))
	if dbkit.IsNoRows(err) {
		return nil, nil //nolint:nilnil // no price is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("current price: %w", err)
	}
	return &p, nil
}

// SetPrice closes the SKU's current price and starts a new one at now.
func (Pricing) SetPrice(ctx context.Context, tx pgx.Tx, tenantID, skuID string, price money.Money, inclusive bool, now time.Time, setBy string) (domain.BasePrice, error) {
	if _, err := tx.Exec(ctx, `UPDATE base_prices SET valid_to = $2 WHERE sku_id = $1 AND valid_to IS NULL`, skuID, now); err != nil {
		return domain.BasePrice{}, fmt.Errorf("close price: %w", err)
	}
	p, err := scanPrice(tx.QueryRow(ctx, `
		INSERT INTO base_prices (tenant_id, sku_id, amount_minor, currency, tax_inclusive, valid_from, set_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+priceColumns,
		tenantID, skuID, price.AmountMinor, price.Currency, inclusive, now, setBy))
	if err != nil {
		return domain.BasePrice{}, fmt.Errorf("set price: %w", err)
	}
	return p, nil
}

// RemovePrice closes the SKU's current price, reporting whether it had one.
func (Pricing) RemovePrice(ctx context.Context, tx pgx.Tx, skuID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE base_prices SET valid_to = $2 WHERE sku_id = $1 AND valid_to IS NULL`, skuID, now)
	if err != nil {
		return false, fmt.Errorf("remove price: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Prices returns the current prices of skuIDs.
func (Pricing) Prices(ctx context.Context, tx pgx.Tx, skuIDs []string) (map[string]domain.BasePrice, error) {
	rows, err := tx.Query(ctx, `SELECT `+priceColumns+` FROM base_prices
		WHERE sku_id = ANY($1::uuid[]) AND valid_to IS NULL`, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("prices: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.BasePrice, len(skuIDs))
	for rows.Next() {
		p, err := scanPrice(rows)
		if err != nil {
			return nil, fmt.Errorf("prices: %w", err)
		}
		out[p.SKUID] = p
	}
	return out, rows.Err()
}

// ListPrices lists current prices by SKU ID after afterSKU.
func (Pricing) ListPrices(ctx context.Context, tx pgx.Tx, afterSKU string, limit int) ([]domain.BasePrice, error) {
	var after *string
	if afterSKU != "" {
		after = &afterSKU
	}
	rows, err := tx.Query(ctx, `SELECT `+priceColumns+` FROM base_prices
		WHERE valid_to IS NULL AND ($1::uuid IS NULL OR sku_id > $1::uuid)
		ORDER BY sku_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.BasePrice, error) { return scanPrice(r) })
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	return out, nil
}

// PriceHistory returns the SKU's prices, newest first. The newest has a
// nil ValidTo when it is current.
func (Pricing) PriceHistory(ctx context.Context, tx pgx.Tx, skuID string, limit int) ([]domain.PricePeriod, error) {
	rows, err := tx.Query(ctx, `SELECT amount_minor, currency, tax_inclusive, valid_from, valid_to, set_by
		FROM base_prices WHERE sku_id = $1 ORDER BY valid_from DESC, valid_to DESC NULLS FIRST LIMIT $2`, skuID, limit)
	if err != nil {
		return nil, fmt.Errorf("price history: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.PricePeriod, error) {
		var p domain.PricePeriod
		err := r.Scan(&p.Price.AmountMinor, &p.Price.Currency, &p.TaxInclusive, &p.ValidFrom, &p.ValidTo, &p.SetBy)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("price history: %w", err)
	}
	return out, nil
}

// TaxClass returns a class with its rules.
func (r Pricing) TaxClass(ctx context.Context, tx pgx.Tx, id string) (domain.TaxClass, error) {
	classes, err := r.TaxClasses(ctx, tx, []string{id})
	if err != nil {
		return domain.TaxClass{}, err
	}
	c, ok := classes[id]
	if !ok {
		return domain.TaxClass{}, ycerrors.NotFound("Tax class", id)
	}
	return c, nil
}

// TaxClasses returns the named classes with their rules, or every class
// when ids is nil.
func (Pricing) TaxClasses(ctx context.Context, tx pgx.Tx, ids []string) (map[string]domain.TaxClass, error) {
	rows, err := tx.Query(ctx, `SELECT class_id, display_name, created_at, updated_at FROM tax_classes
		WHERE $1::text[] IS NULL OR class_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("tax classes: %w", err)
	}
	classes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.TaxClass, error) {
		var c domain.TaxClass
		err := r.Scan(&c.ID, &c.DisplayName, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("tax classes: %w", err)
	}
	out := make(map[string]domain.TaxClass, len(classes))
	for _, c := range classes {
		out[c.ID] = c
	}

	rows, err = tx.Query(ctx, `SELECT class_id, jurisdiction, rule_name, rate::text, base, sort_order FROM tax_rules
		WHERE $1::text[] IS NULL OR class_id = ANY($1) ORDER BY class_id, jurisdiction, sort_order`, ids)
	if err != nil {
		return nil, fmt.Errorf("tax rules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			classID, rate string
			rule          domain.TaxRule
		)
		if err := rows.Scan(&classID, &rule.Jurisdiction, &rule.Name, &rate, &rule.Base, &rule.SortOrder); err != nil {
			return nil, fmt.Errorf("tax rules: %w", err)
		}
		if rule.Rate, err = money.ParseRate(rate); err != nil {
			return nil, fmt.Errorf("tax rules: %w", err)
		}
		c := out[classID]
		c.Rules = append(c.Rules, rule)
		out[classID] = c
	}
	return out, rows.Err()
}

// PutTaxClass creates the class or replaces its name and rules, reporting
// whether it was created.
func (Pricing) PutTaxClass(ctx context.Context, tx pgx.Tx, tenantID, id, displayName string, rules []domain.TaxRule, now time.Time) (domain.TaxClass, bool, error) {
	c := domain.TaxClass{ID: id, DisplayName: displayName, Rules: rules}
	var created bool
	err := tx.QueryRow(ctx, `
		INSERT INTO tax_classes (tenant_id, class_id, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, class_id) DO UPDATE SET display_name = EXCLUDED.display_name, updated_at = EXCLUDED.updated_at
		RETURNING created_at, updated_at, xmax = 0`, tenantID, id, displayName, now).
		Scan(&c.CreatedAt, &c.UpdatedAt, &created)
	if err != nil {
		return domain.TaxClass{}, false, fmt.Errorf("put tax class: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tax_rules WHERE class_id = $1`, id); err != nil {
		return domain.TaxClass{}, false, fmt.Errorf("replace tax rules: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tax_rules (tenant_id, class_id, jurisdiction, rule_name, rate, base, sort_order)
			VALUES ($1, $2, $3, $4, $5::numeric, $6, $7)`,
			tenantID, id, r.Jurisdiction, r.Name, r.Rate.String(), r.Base, r.SortOrder); err != nil {
			return domain.TaxClass{}, false, fmt.Errorf("insert tax rule: %w", err)
		}
	}
	return c, created, nil
}

// DeleteTaxClass deletes a class and its rules.
func (Pricing) DeleteTaxClass(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM tax_classes WHERE class_id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete tax class: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("Tax class", id)
	}
	return nil
}

// CountSKUsInClass counts live SKUs of a class.
func (Pricing) CountSKUsInClass(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM sku_tax_classes WHERE tax_class = $1 AND status <> 'deleted'`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count SKUs in class: %w", err)
	}
	return n, nil
}

// SyncSKU records a SKU's tax class and status as of at, ignoring events
// older than the stored one and never reviving a deleted SKU.
func (Pricing) SyncSKU(ctx context.Context, tx pgx.Tx, tenantID, skuID, taxClass, status string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sku_tax_classes (tenant_id, sku_id, tax_class, status, event_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, sku_id) DO UPDATE SET tax_class = EXCLUDED.tax_class,
			status = EXCLUDED.status, event_at = EXCLUDED.event_at
		WHERE sku_tax_classes.status <> 'deleted' AND sku_tax_classes.event_at <= EXCLUDED.event_at`,
		tenantID, skuID, taxClass, status, at)
	if err != nil {
		return fmt.Errorf("sync SKU: %w", err)
	}
	return nil
}

// DeleteSKU marks a SKU deleted, creating the row if its creation has not
// arrived yet.
func (Pricing) DeleteSKU(ctx context.Context, tx pgx.Tx, tenantID, skuID string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sku_tax_classes (tenant_id, sku_id, tax_class, status, event_at) VALUES ($1, $2, '', 'deleted', $3)
		ON CONFLICT (tenant_id, sku_id) DO UPDATE SET status = 'deleted', event_at = GREATEST(sku_tax_classes.event_at, EXCLUDED.event_at)`,
		tenantID, skuID, at)
	if err != nil {
		return fmt.Errorf("delete SKU: %w", err)
	}
	return nil
}

// SKUTaxClasses returns the tax classes of the live SKUs among skuIDs.
func (Pricing) SKUTaxClasses(ctx context.Context, tx pgx.Tx, skuIDs []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT sku_id, tax_class FROM sku_tax_classes
		WHERE sku_id = ANY($1::uuid[]) AND status <> 'deleted'`, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("SKU tax classes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(skuIDs))
	for rows.Next() {
		var id, class string
		if err := rows.Scan(&id, &class); err != nil {
			return nil, fmt.Errorf("SKU tax classes: %w", err)
		}
		out[id] = class
	}
	return out, rows.Err()
}
