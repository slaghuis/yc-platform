# yc-analytics — Service Requirement

## Overview

`yc-analytics` is the business intelligence service. It consumes events from across the platform and builds **denormalized read models** that answer business questions — revenue, conversion, product performance, customer lifetime value — without burdening operational services. It is the home for cross-service reporting and dashboards.

`yc-analytics` differs from `yc-monitor`:
- **yc-monitor**: operational health, throughput, alerting — "is the platform working?"
- **yc-analytics**: business performance, trends, customer behavior — "is the business working?"

## Location in Monorepo

```
/services/yc-analytics/
```

## Responsibilities

1. Subscribe to business events across the platform
2. Build and maintain denormalized read models (fact + dimension style)
3. Provide a reporting API for dashboards
4. Compute key business KPIs:
   - Revenue (gross, net, by period, by tenant)
   - Order volume
   - Conversion funnel (visit → cart → checkout → order)
   - Cart abandonment rate
   - Average order value
   - Customer lifetime value
   - Product performance (sales count, revenue, return rate)
   - Promotion effectiveness
   - Payment gateway performance (success rate, failure rate)
5. Support ad-hoc querying via a safe query API (restricted)
6. Export data for external BI tools (CSV, Parquet) — future
7. Participate in GDPR cascade by pseudonymizing customer references

## Multi-Tenancy

- All analytics data is tenant-scoped
- Shop admins see only their tenant
- System admins can view platform-wide aggregates (`_platform` scope)
- Cross-tenant comparison views exist only for system admins

## Data Model Approach

Classical dimensional modeling adapted for Postgres:

- **Fact tables**: granular events (one row per order, per cart event, per checkout step, per payment)
- **Dimension tables**: date, tenant, product, category, user cohort, gateway, channel
- Facts link to dimensions by foreign keys
- Postgres with proper indexes handles v1 scale; migration to a columnar store (ClickHouse, DuckDB) is future scope when scan performance demands

## Fact Tables (v1)

| Fact | Grain | Key Dimensions |
|---|---|---|
| `fact_order` | one row per order | date, tenant, user_cohort, status |
| `fact_order_item` | one row per order line | date, tenant, product, sku, category |
| `fact_payment` | one row per payment transaction | date, tenant, gateway, status |
| `fact_checkout_step` | one row per saga state transition | date, tenant, from_state, to_state |
| `fact_cart_event` | one row per cart event | date, tenant, event_type |
| `fact_review` | one row per review | date, tenant, product, rating, status |
| `fact_visitor_session` | one row per visitor session | date, tenant |

## Dimension Tables

| Dimension | Content |
|---|---|
| `dim_date` | calendar table, pre-populated |
| `dim_tenant` | tenant metadata |
| `dim_product` | product snapshot (name, supplier) at time of event |
| `dim_sku` | SKU snapshot |
| `dim_category` | category metadata |
| `dim_user_cohort` | first-order month cohort (per tenant) |
| `dim_gateway` | payment gateway metadata |
| `dim_channel` | messaging channel metadata |

## Pre-Computed Aggregates

For dashboard responsiveness, nightly jobs materialize:

- `agg_daily_revenue` (per tenant, per day)
- `agg_daily_orders` (per tenant, per day, by status)
- `agg_product_performance` (per tenant, per product, per month)
- `agg_conversion_funnel` (per tenant, per day)
- `agg_customer_ltv` (per tenant, per cohort)
- `agg_gateway_performance` (per tenant, per gateway, per day)
- `agg_promotion_effectiveness` (per tenant, per promotion)

## Dashboard Queries

The API supports a fixed set of named queries. Ad-hoc SQL is **not** exposed to shop admins (v1).

| Query | Purpose |
|---|---|
| `revenue_over_time` | Line chart of revenue |
| `top_products` | Ranked products by revenue or volume |
| `conversion_funnel` | Stage-by-stage visitor → order |
| `cart_abandonment_rate` | Over time |
| `average_order_value` | Over time |
| `payment_success_rate` | By gateway, over time |
| `customer_cohort_retention` | Cohort heatmap |
| `review_sentiment` | Average rating over time |

## API

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/analytics/dashboard` | Headline KPIs | shop_admin |
| GET | `/admin/analytics/revenue` | Revenue query | shop_admin |
| GET | `/admin/analytics/orders` | Order metrics | shop_admin |
| GET | `/admin/analytics/products/top` | Top products | shop_admin |
| GET | `/admin/analytics/products/{id}` | Single product performance | shop_admin |
| GET | `/admin/analytics/funnel` | Conversion funnel | shop_admin |
| GET | `/admin/analytics/cohorts` | Cohort retention | shop_admin |
| GET | `/admin/analytics/gateways` | Payment gateway performance | shop_admin |
| GET | `/admin/analytics/promotions` | Promotion effectiveness | shop_admin |
| GET | `/admin/analytics/export/{query}` | CSV export of a named query | shop_admin |

### System Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/sysadmin/analytics/platform` | Platform-wide KPIs | system_admin |
| GET | `/sysadmin/analytics/tenants/comparison` | Cross-tenant comparison | system_admin |
| GET | `/sysadmin/analytics/growth` | Tenant growth metrics | system_admin |

### Internal Endpoints

None in v1. yc-analytics is read-only to the outside; it only consumes events.

## Database Schema

Database: **`yc_analytics`**

```sql
-- Dimensions
CREATE TABLE dim_date (
    date_key        DATE PRIMARY KEY,
    year            INT NOT NULL,
    quarter         INT NOT NULL,
    month           INT NOT NULL,
    month_name      TEXT NOT NULL,
    day             INT NOT NULL,
    day_of_week     INT NOT NULL,
    day_name        TEXT NOT NULL,
    is_weekend      BOOLEAN NOT NULL
);

CREATE TABLE dim_tenant (
    tenant_id       TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    store_currency  TEXT NOT NULL,
    provisioned_at  TIMESTAMPTZ,
    status          TEXT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE dim_product (
    product_id      UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    name_snapshot   TEXT NOT NULL,
    supplier_id     UUID,
    category_ids    UUID[],
    is_digital      BOOLEAN,
    first_seen_at   TIMESTAMPTZ NOT NULL,
    last_seen_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (product_id)
);

CREATE TABLE dim_sku (
    sku_id          UUID PRIMARY KEY,
    product_id      UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    sku_code        TEXT NOT NULL,
    variant_values  JSONB,
    tax_class       TEXT,
    first_seen_at   TIMESTAMPTZ NOT NULL,
    last_seen_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE dim_gateway (
    gateway         TEXT PRIMARY KEY,
    display_name    TEXT NOT NULL
);

CREATE TABLE dim_user_cohort (
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    first_order_date DATE,
    cohort_month    TEXT,                           -- 'YYYY-MM'
    PRIMARY KEY (tenant_id, user_id)
);

-- Facts
CREATE TABLE fact_order (
    order_id              UUID PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    date_key              DATE NOT NULL,
    user_id               TEXT NOT NULL,
    user_type             TEXT NOT NULL,
    cohort_month          TEXT,
    status                TEXT NOT NULL,
    item_count            INT NOT NULL,
    distinct_sku_count    INT NOT NULL,
    gross_amount_minor    BIGINT NOT NULL,
    discount_amount_minor BIGINT NOT NULL,
    tax_amount_minor      BIGINT NOT NULL,
    shipping_amount_minor BIGINT NOT NULL,
    net_amount_minor      BIGINT NOT NULL,
    currency              TEXT NOT NULL,
    gateway               TEXT,
    placed_at             TIMESTAMPTZ NOT NULL,
    delivered_at          TIMESTAMPTZ,
    refunded_amount_minor BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_fact_order_tenant_date ON fact_order (tenant_id, date_key);
CREATE INDEX idx_fact_order_user ON fact_order (tenant_id, user_id);

CREATE TABLE fact_order_item (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id             UUID NOT NULL REFERENCES fact_order(order_id),
    tenant_id            TEXT NOT NULL,
    date_key             DATE NOT NULL,
    sku_id               UUID NOT NULL,
    product_id           UUID NOT NULL,
    quantity             INT NOT NULL,
    unit_price_minor     BIGINT NOT NULL,
    line_total_minor     BIGINT NOT NULL,
    line_tax_minor       BIGINT NOT NULL,
    line_discount_minor  BIGINT NOT NULL
);
CREATE INDEX idx_fact_oi_tenant_date ON fact_order_item (tenant_id, date_key);
CREATE INDEX idx_fact_oi_product ON fact_order_item (tenant_id, product_id, date_key);

CREATE TABLE fact_payment (
    transaction_id   UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    date_key         DATE NOT NULL,
    order_id         UUID,
    gateway          TEXT NOT NULL,
    status           TEXT NOT NULL,
    amount_minor     BIGINT NOT NULL,
    currency         TEXT NOT NULL,
    failure_reason   TEXT,
    event_time       TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fact_payment_tenant_date ON fact_payment (tenant_id, date_key, gateway);

CREATE TABLE fact_checkout_step (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    user_type       TEXT NOT NULL,
    from_state      TEXT,
    to_state        TEXT NOT NULL,
    duration_ms     BIGINT,
    occurred_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fact_cs_tenant_date ON fact_checkout_step (tenant_id, date_key);

CREATE TABLE fact_cart_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    user_id         TEXT NOT NULL,
    user_type       TEXT NOT NULL,
    event_type      TEXT NOT NULL,              -- 'added','removed','abandoned','converted','cleared'
    sku_id          UUID,
    quantity        INT,
    occurred_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fact_cart_tenant_date ON fact_cart_event (tenant_id, date_key, event_type);

CREATE TABLE fact_review (
    review_id       UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    product_id      UUID NOT NULL,
    rating          SMALLINT NOT NULL,
    status          TEXT NOT NULL,
    verified_purchase BOOLEAN,
    occurred_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE fact_visitor_session (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    visitor_token_jti TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fact_vs_tenant_date ON fact_visitor_session (tenant_id, date_key);

-- Aggregates (materialized nightly)
CREATE TABLE agg_daily_revenue (
    tenant_id           TEXT NOT NULL,
    date_key            DATE NOT NULL,
    gross_minor         BIGINT NOT NULL,
    net_minor           BIGINT NOT NULL,
    tax_minor           BIGINT NOT NULL,
    shipping_minor      BIGINT NOT NULL,
    refunded_minor      BIGINT NOT NULL,
    order_count         INT NOT NULL,
    PRIMARY KEY (tenant_id, date_key)
);

CREATE TABLE agg_daily_funnel (
    tenant_id        TEXT NOT NULL,
    date_key         DATE NOT NULL,
    visitors         BIGINT NOT NULL DEFAULT 0,
    carts_created    BIGINT NOT NULL DEFAULT 0,
    checkouts_started BIGINT NOT NULL DEFAULT 0,
    checkouts_confirmed BIGINT NOT NULL DEFAULT 0,
    orders_placed    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, date_key)
);

CREATE TABLE agg_product_performance (
    tenant_id       TEXT NOT NULL,
    product_id      UUID NOT NULL,
    month_key       TEXT NOT NULL,              -- 'YYYY-MM'
    units_sold      BIGINT NOT NULL,
    revenue_minor   BIGINT NOT NULL,
    return_count    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, product_id, month_key)
);

CREATE TABLE agg_gateway_performance (
    tenant_id        TEXT NOT NULL,
    gateway          TEXT NOT NULL,
    date_key         DATE NOT NULL,
    attempts         BIGINT NOT NULL,
    successes        BIGINT NOT NULL,
    failures         BIGINT NOT NULL,
    total_amount_minor BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, gateway, date_key)
);

CREATE TABLE agg_cohort_retention (
    tenant_id        TEXT NOT NULL,
    cohort_month     TEXT NOT NULL,
    months_since     INT NOT NULL,
    active_customers BIGINT NOT NULL,
    revenue_minor    BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, cohort_month, months_since)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE fact_order ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fact_order
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Aggregation Jobs

- **Realtime (event-driven)**: facts are written as events arrive
- **Nightly batch (configurable cron)**:
  - Rebuild `agg_daily_revenue` for yesterday
  - Rebuild `agg_daily_funnel` for yesterday
  - Rebuild `agg_product_performance` for current month
  - Rebuild `agg_gateway_performance` for yesterday
  - Rebuild `agg_cohort_retention` monthly
- **Backfill**: aggregates can be rebuilt from facts at any time; facts are the source of truth

## Published Events

| Event | When |
|---|---|
| `analytics.aggregate.rebuilt.v1` | Aggregate job completed |
| `analytics.milestone.reached.v1` | Tenant hits a milestone (first order, 100th order, R100k revenue) — triggers congratulatory messaging |

## Subscribed Events

Broad fan-in. Each event maps to one or more fact table inserts.

| Event | Fact Updated |
|---|---|
| `platform.tenant.provisioned.v1` | `dim_tenant` |
| `platform.tenant.configuration_changed.v1` | `dim_tenant` |
| `product.created.v1` / `.updated.v1` | `dim_product` |
| `product.sku.created.v1` / `.updated.v1` | `dim_sku` |
| `identity.visitor.created.v1` | `fact_visitor_session` |
| `cart.created.v1`, `cart.item.added.v1`, `cart.abandoned.v1`, `cart.converted.v1`, `cart.cleared.v1` | `fact_cart_event` |
| `checkout.started.v1`, `checkout.priced.v1`, `checkout.confirmed.v1`, `checkout.completed.v1`, `checkout.failed.v1`, `checkout.expired.v1` | `fact_checkout_step` |
| `order.placed.v1` | `fact_order`, `fact_order_item`, update `dim_user_cohort` (first order detection) |
| `order.cancelled.v1` | Update `fact_order.status` |
| `order.delivered.v1` | Update `fact_order.delivered_at` |
| `order.refunded.v1` | Update `fact_order.refunded_amount_minor` |
| `payment.authorized.v1`, `payment.captured.v1`, `payment.charged.v1`, `payment.authorization_failed.v1`, `payment.capture_failed.v1`, `payment.charge_failed.v1` | `fact_payment` |
| `review.submitted.v1`, `review.approved.v1`, `review.rejected.v1` | `fact_review` |
| `identity.user.deleted.v1` | Pseudonymize `user_id` in facts; publish `platform.gdpr.cascade_completed.v1` |
| `platform.tenant.deleted.v1` | Mark tenant inactive in `dim_tenant`; retain facts for reporting history or delete per policy |

## Configuration

```yaml
yc-analytics:
  listen: :8080
  database:
    url: postgres://yc_analytics:...@...
  aggregation:
    nightly_cron: "0 2 * * *"           # 02:00 daily
    cohort_cron: "0 3 1 * *"            # monthly on the 1st
    backfill_batch_size: 1000
  retention:
    facts_days: 2555                     # ~7 years, per ADR-004 for financial facts
    aggregates_days: 2555
    visitor_session_days: 90
  milestones:
    first_order: true
    revenue_tiers_minor:
      - 10000000                         # R100k
      - 100000000                        # R1M
      - 1000000000                       # R10M
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None at startup** (consume-only via events)

## Called By

- Shop admins (dashboards)
- System admins (platform analytics)
- Future: external BI tools (via export)

## GDPR Behavior

- Facts contain `user_id` (PII link)
- On `identity.user.deleted.v1`:
  - Replace `user_id` with pseudonym from yc-identity across all fact tables
  - Retain all facts for financial reporting integrity (ADR-004: 7 years)
  - Aggregates do not contain `user_id` directly — rebuilt from pseudonymized facts next run
- Publish `platform.gdpr.cascade_completed.v1`

## Open Questions / Future Scope

- Migration to columnar store (ClickHouse, DuckDB) when scale demands
- Ad-hoc SQL for shop admins (sandboxed)
- Scheduled reports delivered by yc-message
- External BI integration (Metabase, Looker, Power BI)
- Predictive analytics (churn prediction, LTV forecast)
- A/B test analysis framework
- Marketing attribution models
- Real-time dashboard streaming (websockets)
- Cross-tenant benchmarks (anonymized industry comparisons)
- Shopify-style storefront analytics (page views, bounce rate) — requires frontend telemetry service
