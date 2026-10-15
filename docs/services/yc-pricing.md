# yc-pricing — Service Requirement

## Overview

`yc-pricing` is the authoritative source for prices. It maintains a base price per SKU in the tenant's store currency and runs a pipeline of evaluators to produce the final price for a given context: currency conversion, promotions, discounts, and taxes (ADR-016). It emits a `PricingResult` with a complete audit trail.

## Location in Monorepo

```
/services/yc-pricing/
```

## Responsibilities

1. Maintain base prices per SKU in tenant store currency
2. Maintain promotion rules (percentage/fixed discount, time-bounded, SKU/category/cart-level)
3. Maintain tenant tax configuration (classes and rules per jurisdiction — ADR-016)
4. Maintain exchange rates (cached from external rate provider)
5. Run the pricing pipeline for a given PricingContext and return PricingResult
6. Expose a simple price-fetching API for display (cached)
7. Expose a full-pipeline API for checkout (authoritative, uncached)
8. Emit price-change and promotion events

## Multi-Tenancy

- All pricing data is tenant-scoped
- Each tenant has: one store currency (ADR-014), one set of tax classes/rules, one set of promotions, one set of base prices

## Pricing Context

```json
{
  "tenant_id": "shop-abc",
  "sku_ids": ["...", "..."],      // or line items with quantity
  "quantities": {"sku-x": 2},
  "user_id": "...",               // for user-specific pricing (future)
  "display_currency": "USD",       // for display conversion
  "delivery_country_code": "ZA",   // for tax jurisdiction
  "promotion_codes": ["SAVE10"],
  "channel": "web",
  "evaluated_at": "2025-01-15T10:00:00Z"
}
```

## Pricing Pipeline (Order of Evaluators)

Evaluators run in a defined order; each produces an audit entry.

1. **BasePriceEvaluator** — resolves base price per SKU in store currency
2. **BulkDiscountEvaluator** — quantity-based discounts (future)
3. **CategoryPromotionEvaluator** — category-level promotions
4. **SKUPromotionEvaluator** — SKU-specific promotions
5. **PromotionCodeEvaluator** — coupon code application
6. **CartPromotionEvaluator** — cart-total-based (e.g., "spend 500, get 50 off")
7. **TaxEvaluator** — applies tax classes per SKU, per jurisdiction (ADR-016); produces per-line tax breakdown
8. **DisplayCurrencyEvaluator** — final step; converts store currency amounts to display currency for presentation only (does **not** replace store currency values)

Each evaluator is a Go interface:

```go
type Evaluator interface {
    Name() string
    Priority() int
    Apply(ctx context.Context, pc *PricingContext, pr *PricingResult) error
}
```

Evaluators are registered in a configurable pipeline. Order is tenant-overridable (future), but v1 uses the fixed order above.

## Pricing Result

```json
{
  "pricing_context": { ... },
  "store_currency": "ZAR",
  "line_items": [
    {
      "sku_id": "...",
      "quantity": 2,
      "base_unit_price": 10000,
      "line_subtotal": 20000,
      "discounts": [
        { "name": "Summer Sale", "type": "sku_promotion", "amount": -1000 }
      ],
      "line_pre_tax": 19000,
      "taxes": [
        { "name": "VAT", "rate": 0.15, "class": "za-standard", "amount": 2478 }
      ],
      "line_total": 19000
    }
  ],
  "totals": {
    "subtotal_pre_discount": 20000,
    "discount_total": -1000,
    "subtotal_pre_tax": 19000,
    "tax_total": 2478,
    "grand_total": 19000
  },
  "display": {
    "currency": "USD",
    "rate": 0.054,
    "grand_total": 1026
  },
  "audit_trail": [
    { "evaluator": "BasePriceEvaluator", "detail": {...} },
    { "evaluator": "SKUPromotionEvaluator", "detail": {...} },
    { "evaluator": "TaxEvaluator", "detail": {...} },
    { "evaluator": "DisplayCurrencyEvaluator", "detail": {...} }
  ],
  "evaluated_at": "2025-01-15T10:00:00Z"
}
```

- All monetary values in **minor units** (cents) to avoid float arithmetic
- Tax-inclusive pricing (ADR-016) is handled inside TaxEvaluator; the audit trail distinguishes extraction (inclusive) from addition (exclusive)

## Exchange Rates

- Fetched from a configurable provider (Frankfurter by default; `exchangerate.host` with an access key)
- Cached in Postgres with timestamp
- TTL: 1 hour (configurable)
- On fetch failure: use the last-known rate; log a warning; after 24 hours of staleness, emit `pricing.exchange_rates_stale.v1` for alerting
- Rates are **not** stored on orders — orders are in store currency

## API

### Public Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/prices/sku/{id}` | Get display price for one SKU (cached) | any |
| POST | `/prices/evaluate` | Full pipeline evaluation for a context | any |
| GET | `/promotions/active` | List active promotions (public) | any |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| PUT | `/admin/prices/sku/{id}` | Set base price for SKU | shop_admin |
| GET | `/admin/prices` | List base prices | shop_admin |
| POST | `/admin/promotions` | Create promotion | shop_admin |
| PUT | `/admin/promotions/{id}` | Update promotion | shop_admin |
| DELETE | `/admin/promotions/{id}` | Delete promotion | shop_admin |
| GET | `/admin/tax-classes` | List tax classes | shop_admin |
| PUT | `/admin/tax-classes` | Update tax classes & rules | shop_admin |
| GET | `/admin/pricing-config` | Get tenant pricing config (currency, inclusive/exclusive) | shop_admin |
| PUT | `/admin/pricing-config` | Update tenant pricing config | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/evaluate` | Full pricing evaluation (used by yc-checkout, yc-cart) | service |
| GET | `/internal/prices/sku/{id}` | Base price lookup | service |

### Phase 2 Implementation Notes

The contract is `services/yc-pricing/api/openapi.yaml`. Differences from the tables above:

- **Pipeline** is BasePrice → Tax → DisplayCurrency. Promotions, the `/promotions` endpoints and the internal endpoints arrive in Phase 3.
- **`POST /prices/evaluate`** takes `{"lines": [{"sku_id", "quantity"}], "jurisdiction"?, "display_currency"?}`. SKUs that cannot be priced are returned in `unpriced` with a reason (`no_base_price`, `unknown_sku`, `unknown_tax_class`) and left out of the totals. `GET /prices/sku/{id}` evaluates quantity 1 and returns 404 when the SKU is unpriced. Responses are not cached yet.
- **Display currency** comes from the body, then the `display_currency` query parameter, then the `X-YC-Display-Currency` header. A currency with no known rate is a 400 (`display_currency: unavailable`). Converted amounts are in `display`; store-currency amounts never change.
- **Tax-inclusive meaning is per price.** A base price records the shop's `tax_inclusive` setting when it was set, and is always evaluated that way. Changing the setting affects only prices set afterwards. `tax_inclusive` in the result is the shop's display preference (gross when true, net when false).
- **Tax maths.** Taxes are computed on line totals and rounded half away from zero. Inclusive prices are divided by the combined multiplier (1 + Σ effective rates, compounding included), so R200.00 at 15% VAT gives net R173.91 and VAT R26.09. The last rule absorbs any rounding cent, so net + tax = gross exactly. Rules for a subdivision (`ZA-WC`) fall back to the country (`ZA`). A class with no rules for the jurisdiction is untaxed, and the audit trail notes it.
- **Tax classes** are managed one at a time: `PUT /admin/tax-classes/{class_id}` creates (201) or replaces (200) the name and all rules, and `DELETE` refuses a class that live SKUs use (409). `GET /admin/prices/sku/{id}` returns the price history.
- **The store currency cannot change while base prices exist** (409). A shop's config is created from `platform.tenant.provisioned.v1` and never overwritten by redelivery.
- **SKU tax classes** come from `product.sku.created/updated/deleted.v1`, consumed by one durable consumer so they apply in order. Older events are ignored, and a deleted SKU is never revived. Deleting a SKU (or a product, through `product.deleted.v1`) removes its base price and publishes `pricing.base_price.removed.v1`.
- **Exchange rates.** The default provider is Frankfurter (ECB reference rates, no key); `exchangerate_host` needs `rates.exchangerate_host.access_key`. Rates for every store currency (plus `rates.base_currencies`) are refreshed at startup and then hourly; a new or changed store currency is fetched straight away, through yc-pricing's own consumer of `pricing.config.changed.v1`. Only the latest rate per pair is kept; a missing pair is derived from its inverse. When fetches have failed for `rates.stale_after` (24h), `pricing.exchange_rates_stale.v1` is published at most once per period, and the last known rate is still used.
- The service registers its routes with yc-gateway at startup (`gateway.internal_url`).

### Phase 3 Implementation Notes

- **`POST /internal/prices/evaluate`** is the service-to-service form of `POST /prices/evaluate` (same body and result), reached only through yc-gateway's internal listener with a service token (ADR-021). yc-cart uses it to snapshot and re-price cart lines. The display currency comes from the body, then `X-YC-Display-Currency`; the gateway forwards the calling service (`X-YC-Caller-Service`) and the tenant the service acts for. `/internal/prices/sku/{id}` is not implemented; callers evaluate quantity 1.
- **Promotions** are still not in the pipeline; they arrive with yc-promotion.

## Database Schema

Database: **`yc_pricing`**

```sql
CREATE TABLE tenant_pricing_config (
    tenant_id            TEXT PRIMARY KEY,
    store_currency       TEXT NOT NULL,
    tax_inclusive        BOOLEAN NOT NULL DEFAULT TRUE,
    default_jurisdiction TEXT NOT NULL,          -- 'ZA'
    rate_provider        TEXT NOT NULL DEFAULT 'exchangerate_host',
    rate_ttl_seconds     INT NOT NULL DEFAULT 3600,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE base_prices (
    tenant_id       TEXT NOT NULL,
    sku_id          UUID NOT NULL,
    price_minor     BIGINT NOT NULL,         -- in store currency, minor units
    valid_from      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    valid_to        TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, sku_id, valid_from)
);
CREATE INDEX idx_base_prices_current ON base_prices (tenant_id, sku_id)
    WHERE valid_to IS NULL;

CREATE TABLE tax_classes (
    tenant_id       TEXT NOT NULL,
    class_id        TEXT NOT NULL,           -- e.g. 'za-standard'
    display_name    TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, class_id)
);

CREATE TABLE tax_rules (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    class_id        TEXT NOT NULL,
    jurisdiction    TEXT NOT NULL,           -- 'ZA', 'ZA-WC', etc.
    rule_name       TEXT NOT NULL,
    rate            NUMERIC(10,6) NOT NULL,
    base            TEXT NOT NULL,           -- 'line_total','unit_quantity','line_total_including_<name>'
    sort_order      INT NOT NULL,
    valid_from      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    valid_to        TIMESTAMPTZ,
    FOREIGN KEY (tenant_id, class_id) REFERENCES tax_classes(tenant_id, class_id)
);
CREATE INDEX idx_tax_rules_lookup ON tax_rules (tenant_id, class_id, jurisdiction);

CREATE TABLE promotions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL,           -- 'sku','category','cart','code'
    discount_type   TEXT NOT NULL,           -- 'percentage','fixed'
    discount_value  NUMERIC(12,4) NOT NULL,
    scope           JSONB NOT NULL,          -- SKU/category IDs, code, min cart total, etc.
    priority        INT NOT NULL DEFAULT 100,
    stackable       BOOLEAN NOT NULL DEFAULT FALSE,
    valid_from      TIMESTAMPTZ NOT NULL,
    valid_to        TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_promotions_tenant ON promotions (tenant_id, status, valid_from, valid_to);

CREATE TABLE exchange_rates (
    base_currency   TEXT NOT NULL,
    quote_currency  TEXT NOT NULL,
    rate            NUMERIC(20,10) NOT NULL,
    fetched_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (base_currency, quote_currency, fetched_at)
);
CREATE INDEX idx_exchange_rates_latest ON exchange_rates (base_currency, quote_currency, fetched_at DESC);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE base_prices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON base_prices
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Published Events

| Event | When |
|---|---|
| `pricing.base_price.set.v1` | Base price created or changed |
| `pricing.base_price.removed.v1` | Base price removed |
| `pricing.tax_class.created.v1` | New tax class defined |
| `pricing.tax_class.updated.v1` | Tax class rules changed |
| `pricing.tax_class.deleted.v1` | Tax class removed |
| `pricing.promotion.created.v1` | Promotion defined |
| `pricing.promotion.started.v1` | Promotion becomes active (scheduled or manual) |
| `pricing.promotion.ended.v1` | Promotion ends |
| `pricing.promotion.updated.v1` | Promotion changed |
| `pricing.promotion.deleted.v1` | Promotion removed |
| `pricing.config.changed.v1` | Tenant pricing config changed |
| `pricing.exchange_rates_updated.v1` | Rate cache refreshed |
| `pricing.exchange_rates_stale.v1` | Rate fetch has failed for > 24h |

## Subscribed Events

| Event | Action |
|---|---|
| `product.sku.created.v1` | Create a `base_prices` row with 0 (or default); shop_admin must set real price |
| `product.sku.updated.v1` | Track the SKU's tax class and status |
| `product.sku.deleted.v1` | Expire `base_prices` and any promotions targeting the SKU |
| `product.deleted.v1` | Expire the base prices of the product's SKUs |
| `pricing.config.changed.v1` (its own) | Fetch exchange rates for a new store currency straight away |
| `platform.tenant.provisioned.v1` | Create `tenant_pricing_config` with defaults |
| `platform.tenant.deleted.v1` | Delete all pricing data for tenant |

## Configuration

```yaml
yc-pricing:
  listen: :8080
  database:
    url: postgres://yc_pricing:...@...
  rates:
    provider: exchangerate_host
    api_key: ...
    refresh_interval_seconds: 3600
    stale_warning_hours: 24
  cache:
    sku_price_ttl_seconds: 60
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None at startup**
- External: rate provider API

## Called By

- `yc-cart` (for cart display)
- `yc-checkout` (authoritative pricing at PRICING step)
- `yc-product` (for search projection via events)
- Clients (for product page display)

## GDPR Behavior

- No PII stored
- Pricing results are not persisted per user — they are computed per request
- No cascade subscription needed

## Open Questions / Future Scope

- Customer-group pricing (B2B tiered pricing)
- Loyalty discounts
- Dynamic pricing (demand-based)
- Minimum advertised price enforcement
- Historical price tracking for shop admin reporting
- Dedicated yc-tax service extraction (ADR-016 Future Evolution)
- Bulk price import/export CSV
