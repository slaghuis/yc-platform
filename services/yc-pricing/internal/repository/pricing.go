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

// SyncSKU records a SKU's product, tax class and status as of at, ignoring
// events older than the stored one and never reviving a deleted SKU.
func (Pricing) SyncSKU(ctx context.Context, tx pgx.Tx, tenantID, skuID, productID, taxClass, status string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sku_tax_classes (tenant_id, sku_id, tax_class, status, event_at, product_id)
		VALUES ($1, $2, $3, $4, $5, $6::uuid)
		ON CONFLICT (tenant_id, sku_id) DO UPDATE SET tax_class = EXCLUDED.tax_class,
			status = EXCLUDED.status, event_at = EXCLUDED.event_at, product_id = EXCLUDED.product_id
		WHERE sku_tax_classes.status <> 'deleted' AND sku_tax_classes.event_at <= EXCLUDED.event_at`,
		tenantID, skuID, taxClass, status, at, nullUUID(productID))
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

// SKUTaxClasses returns the tax classes and product IDs of the live SKUs
// among skuIDs. productIDs omits SKUs whose product_id is still unknown.
func (Pricing) SKUTaxClasses(ctx context.Context, tx pgx.Tx, skuIDs []string) (taxClasses, productIDs map[string]string, err error) {
	rows, err := tx.Query(ctx, `SELECT sku_id, tax_class, COALESCE(product_id::text, '') FROM sku_tax_classes
		WHERE sku_id = ANY($1::uuid[]) AND status <> 'deleted'`, skuIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("SKU tax classes: %w", err)
	}
	defer rows.Close()
	taxClasses = make(map[string]string, len(skuIDs))
	productIDs = make(map[string]string, len(skuIDs))
	for rows.Next() {
		var id, class, productID string
		if err := rows.Scan(&id, &class, &productID); err != nil {
			return nil, nil, fmt.Errorf("SKU tax classes: %w", err)
		}
		taxClasses[id] = class
		if productID != "" {
			productIDs[id] = productID
		}
	}
	return taxClasses, productIDs, rows.Err()
}

const saleColumns = `id, sku_id, amount_minor, currency, tax_inclusive, name, starts_at, ends_at, updated_at`

func scanSale(row pgx.Row) (domain.SalePrice, error) {
	var s domain.SalePrice
	err := row.Scan(&s.ID, &s.SKUID, &s.Price.AmountMinor, &s.Price.Currency, &s.TaxInclusive,
		&s.Name, &s.StartsAt, &s.EndsAt, &s.UpdatedAt)
	return s, err
}

// SetSalePrice creates or replaces the SKU's sale overlay.
func (Pricing) SetSalePrice(ctx context.Context, tx pgx.Tx, tenantID, skuID string, price money.Money, inclusive bool, in domain.SalePriceInput, now time.Time) (domain.SalePrice, error) {
	s, err := scanSale(tx.QueryRow(ctx, `
		INSERT INTO sale_prices (tenant_id, sku_id, amount_minor, currency, tax_inclusive, name, starts_at, ends_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT (tenant_id, sku_id) DO UPDATE SET amount_minor = EXCLUDED.amount_minor,
			currency = EXCLUDED.currency, tax_inclusive = EXCLUDED.tax_inclusive, name = EXCLUDED.name,
			starts_at = EXCLUDED.starts_at, ends_at = EXCLUDED.ends_at, updated_at = EXCLUDED.updated_at
		RETURNING `+saleColumns,
		tenantID, skuID, price.AmountMinor, price.Currency, inclusive, in.Name, in.StartsAt, in.EndsAt, now))
	if err != nil {
		return domain.SalePrice{}, fmt.Errorf("set sale price: %w", err)
	}
	return s, nil
}

// GetSalePrice returns the SKU's sale overlay, if any.
func (Pricing) GetSalePrice(ctx context.Context, tx pgx.Tx, skuID string) (*domain.SalePrice, error) {
	s, err := scanSale(tx.QueryRow(ctx, `SELECT `+saleColumns+` FROM sale_prices WHERE sku_id = $1`, skuID))
	if dbkit.IsNoRows(err) {
		return nil, nil //nolint:nilnil // no sale is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("get sale price: %w", err)
	}
	return &s, nil
}

// RemoveSalePrice deletes the SKU's sale overlay, reporting whether it had one.
func (Pricing) RemoveSalePrice(ctx context.Context, tx pgx.Tx, skuID string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM sale_prices WHERE sku_id = $1`, skuID)
	if err != nil {
		return false, fmt.Errorf("remove sale price: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ActiveSales returns overlays for skuIDs that are active at now.
func (Pricing) ActiveSales(ctx context.Context, tx pgx.Tx, skuIDs []string, now time.Time) (map[string]domain.SalePrice, error) {
	rows, err := tx.Query(ctx, `SELECT `+saleColumns+` FROM sale_prices
		WHERE sku_id = ANY($1::uuid[]) AND starts_at <= $2 AND (ends_at IS NULL OR ends_at > $2)`, skuIDs, now)
	if err != nil {
		return nil, fmt.Errorf("active sales: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.SalePrice, len(skuIDs))
	for rows.Next() {
		s, err := scanSale(rows)
		if err != nil {
			return nil, fmt.Errorf("active sales: %w", err)
		}
		out[s.SKUID] = s
	}
	return out, rows.Err()
}

const bxgyColumns = `id, name, COALESCE(qualifying_product_id::text, ''), COALESCE(qualifying_sku_id::text, ''),
	COALESCE(target_product_id::text, ''), COALESCE(target_sku_id::text, ''),
	discount_type, discount_value, starts_at, ends_at, enabled, created_at, updated_at`

func scanBXGY(row pgx.Row) (domain.BXGYCampaign, error) {
	var c domain.BXGYCampaign
	err := row.Scan(&c.ID, &c.Name, &c.QualifyingProductID, &c.QualifyingSKUID,
		&c.TargetProductID, &c.TargetSKUID, &c.DiscountType, &c.DiscountValue,
		&c.StartsAt, &c.EndsAt, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// CreateBXGY inserts a buy-X-get-Y campaign.
func (Pricing) CreateBXGY(ctx context.Context, tx pgx.Tx, tenantID string, in domain.BXGYInput, now time.Time) (domain.BXGYCampaign, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	c, err := scanBXGY(tx.QueryRow(ctx, `
		INSERT INTO bxgy_campaigns (
			tenant_id, name, qualifying_product_id, qualifying_sku_id, target_product_id, target_sku_id,
			discount_type, discount_value, starts_at, ends_at, enabled, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4::uuid, $5::uuid, $6::uuid, $7, $8, $9, $10, $11, $12, $12)
		RETURNING `+bxgyColumns,
		tenantID, in.Name, nullUUID(in.QualifyingProductID), nullUUID(in.QualifyingSKUID),
		nullUUID(in.TargetProductID), nullUUID(in.TargetSKUID),
		in.DiscountType, in.DiscountValue, in.StartsAt, in.EndsAt, enabled, now))
	if err != nil {
		return domain.BXGYCampaign{}, fmt.Errorf("create BXGY: %w", err)
	}
	return c, nil
}

// UpdateBXGY replaces a campaign's fields. When in.Enabled is nil the
// existing enabled flag is kept.
func (Pricing) UpdateBXGY(ctx context.Context, tx pgx.Tx, id string, in domain.BXGYInput, now time.Time) (domain.BXGYCampaign, error) {
	c, err := scanBXGY(tx.QueryRow(ctx, `
		UPDATE bxgy_campaigns SET name = $2, qualifying_product_id = $3::uuid, qualifying_sku_id = $4::uuid,
			target_product_id = $5::uuid, target_sku_id = $6::uuid, discount_type = $7, discount_value = $8,
			starts_at = $9, ends_at = $10, enabled = COALESCE($11, enabled), updated_at = $12
		WHERE id = $1 RETURNING `+bxgyColumns,
		id, in.Name, nullUUID(in.QualifyingProductID), nullUUID(in.QualifyingSKUID),
		nullUUID(in.TargetProductID), nullUUID(in.TargetSKUID),
		in.DiscountType, in.DiscountValue, in.StartsAt, in.EndsAt, in.Enabled, now))
	if dbkit.IsNoRows(err) {
		return domain.BXGYCampaign{}, ycerrors.NotFound("BXGY campaign", id)
	}
	if err != nil {
		return domain.BXGYCampaign{}, fmt.Errorf("update BXGY: %w", err)
	}
	return c, nil
}

// GetBXGY returns a campaign by ID.
func (Pricing) GetBXGY(ctx context.Context, tx pgx.Tx, id string) (domain.BXGYCampaign, error) {
	c, err := scanBXGY(tx.QueryRow(ctx, `SELECT `+bxgyColumns+` FROM bxgy_campaigns WHERE id = $1`, id))
	if dbkit.IsNoRows(err) {
		return domain.BXGYCampaign{}, ycerrors.NotFound("BXGY campaign", id)
	}
	if err != nil {
		return domain.BXGYCampaign{}, fmt.Errorf("get BXGY: %w", err)
	}
	return c, nil
}

// ListBXGY lists campaigns ordered by name.
func (Pricing) ListBXGY(ctx context.Context, tx pgx.Tx) ([]domain.BXGYCampaign, error) {
	rows, err := tx.Query(ctx, `SELECT `+bxgyColumns+` FROM bxgy_campaigns ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list BXGY: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.BXGYCampaign, error) { return scanBXGY(r) })
	if err != nil {
		return nil, fmt.Errorf("list BXGY: %w", err)
	}
	return out, nil
}

// DeleteBXGY deletes a campaign.
func (Pricing) DeleteBXGY(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM bxgy_campaigns WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete BXGY: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("BXGY campaign", id)
	}
	return nil
}

// ActiveCampaigns returns enabled BXGY campaigns active at now.
func (Pricing) ActiveCampaigns(ctx context.Context, tx pgx.Tx, now time.Time) ([]domain.BXGYCampaign, error) {
	rows, err := tx.Query(ctx, `SELECT `+bxgyColumns+` FROM bxgy_campaigns
		WHERE enabled AND starts_at <= $1 AND (ends_at IS NULL OR ends_at > $1) ORDER BY name, id`, now)
	if err != nil {
		return nil, fmt.Errorf("active campaigns: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.BXGYCampaign, error) { return scanBXGY(r) })
	if err != nil {
		return nil, fmt.Errorf("active campaigns: %w", err)
	}
	return out, nil
}

func nullUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
