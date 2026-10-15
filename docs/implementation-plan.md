# Implementation Plan — yc-platform v1

A pragmatic, dependency-ordered build plan that gets us to a working platform with the shortest possible critical path. Each phase ends with a demonstrable milestone.

---

## Guiding Principles

1. **Walking skeleton first** — get an end-to-end flow working with minimal functionality, then add features. Don't finish any one service before touching others.
2. **Dependency-ordered** — a service is built when its dependencies exist (or are mocked with a plan to replace).
3. **Vertical slices, not horizontal layers** — each phase delivers a user-visible capability, not "all databases first, then all APIs."
4. **Shared libraries proven by first use** — build `yc-eventbus` as part of Phase 1 against yc-identity, not in isolation.
5. **Testing from phase 1** — unit tests always, integration tests from the first service, e2e tests from the first saga.
6. **Observable from day one** — logging, tracing, health checks baked into every scaffold.
7. **Deployable, not just runnable** — Docker images and compose stack working from the end of Phase 1.
8. **No "we'll fix it later" shortcuts for cross-cutting concerns** — auth, tenancy, outbox, GDPR hooks exist from the first commit of each service.

---

## Phase Overview

| Phase | Duration (indicative) | Milestone |
|---|---|---|
| **0. Foundations** | 1 week | Monorepo scaffolded; dev stack runs |
| **1. Walking skeleton** | 2 weeks | Visitor can authenticate; one event flows end-to-end |
| **2. Catalog + browse** | 2 weeks | Visitor can see products with prices |
| **3. Cart + authenticated users** | 1.5 weeks | Customer can register, log in, build a cart |
| **4. Checkout happy path** | 3 weeks | Customer completes a Stripe checkout end-to-end |
| **5. Yoco + the real South African flow** | 1 week | Checkout works with Yoco (variant B) |
| **6. Order fulfillment** | 2 weeks | Shop admin ships; customer sees status |
| **7. Reviews + preferences** | 1 week | Customers can review, change preferences |
| **8. Operational maturity** | 2 weeks | Monitor, Audit, Analytics running; GDPR flows tested |
| **9. Hardening** | 2 weeks | Security review, load test, polish |

Total: approximately **16–17 weeks**, assuming one capable engineer plus AI code factory assistance, allowing for calendar slip.

---

## Phase 0 — Foundations (1 week)

Deliver the monorepo so the AI code factory has a stable target.

### Scope

- Repo skeleton per the monorepo layout document
- `go.work` configured, all empty modules created
- Docker Compose dev stack (Postgres, NATS JetStream, S3-compatible storage via SeaweedFS, Jaeger, MailHog)
- `postgres-init.sh` creates one DB per service
- Task runner (`Taskfile.yml`) with core tasks: `bootstrap`, `devstack:up`, `migrate:up`, `test`, `lint`
- `.golangci.yml`, `.editorconfig`, `.pre-commit-config.yaml`
- GitHub Actions CI: lint, unit test, schema validation
- `scripts/gen-service.sh` scaffolder working for an empty service
- Standards documents in `docs/standards/`:
  - `go-conventions.md`
  - `api-conventions.md`
  - `error-handling.md`
  - `logging-tracing.md`
  - `testing.md`
  - `event-conventions.md`
- Developer onboarding doc that someone external can follow in under an hour

### Not in scope

- Any service code
- Any shared library implementation (just empty modules)
- Any event schemas

### Milestone

```
task bootstrap
task devstack:up
task scaffold:service NAME=yc-example
task run SERVICE=yc-example   # starts, logs "hello", exits cleanly on SIGTERM
```

### Exit criteria

- Fresh checkout, run `task bootstrap && task devstack:up` → everything up in under 2 minutes
- `task test` passes (no tests yet, but command works)
- `task lint` passes
- CI runs on PR

---

## Phase 1 — Walking Skeleton (2 weeks)

Prove the end-to-end mechanics: a visitor hits yc-gateway, gets a token from yc-identity, triggers an event that flows through NATS to a consumer, which writes to its own database. The whole observability stack lights up with traces, logs, and metrics.

This phase is the hardest because every foundational decision is exercised for the first time. Resist expanding scope.

### Scope — Shared libraries (built, tested, documented)

- `yc-telemetry` — slog logger, OpenTelemetry setup, trace/log correlation
- `yc-config` — koanf-based YAML + env loader
- `yc-errors` — error taxonomy with HTTP mapping
- `yc-httpkit` — chi-based server, middleware chain, graceful shutdown, health endpoints
- `yc-dbkit` — pgx pool, migrations, RLS session helper
- `yc-authctx` — context-propagated auth, middleware
- `yc-eventbus` — NATS JetStream publishing, outbox relay, consumer idempotency, DLQ (minimum viable; full feature set evolves)
- `yc-testkit` — testcontainers helpers for Postgres and NATS

### Scope — Event schemas (first batch)

- `envelope/v1.json`
- `common/actor.v1.json`
- `platform/service.registered.v1.json`
- `platform/service.deregistered.v1.json`
- `platform/tenant.provisioned.v1.json`
- `identity/visitor.created.v1.json`
- `identity/user.logged_in.v1.json`

Plus the schema-to-Go generator (`tools/yc-schema-gen`) producing types into `libs/yc-events`.

### Scope — Services

**yc-identity (minimum viable)**
- Issue visitor tokens
- Validate tokens (internal endpoint)
- Postgres schema and migrations
- Publishes `identity.visitor.created.v1`
- JWT signing (RS256)

**yc-gateway (minimum viable)**
- Static route configuration from YAML (no dynamic service registration yet)
- Visitor token issuance on unauthenticated requests
- Token validation via yc-identity
- Request forwarding to backend
- Correlation ID generation
- Tenant resolution by hostname

**yc-monitor (stub)**
- Consumes `identity.visitor.created.v1` and records to its own database
- Exposes `/sysadmin/events` to prove the event was received
- Not full-featured; just proves the event chain works

### Deferred

- Service self-registration with gateway (static for now)
- User registration, OAuth, roles beyond visitor
- Any catalog, pricing, cart

### Milestone

Scripted demo:
1. `curl` an unauthenticated request through yc-gateway
2. Response includes a visitor token (cookie or body)
3. In Jaeger: a trace spanning gateway → identity
4. In the monitor DB: a row for the `identity.visitor.created.v1` event
5. Logs correlated across services via correlation ID
6. All three services healthy at `/healthz`

### Exit criteria

- End-to-end request works and is traced
- Outbox pattern verified: kill yc-identity between DB write and event publish, restart, event appears
- DLQ verified: force a bad message, confirm it lands in DLQ topic
- One tenant provisioned via a seed migration or script
- A reference service pattern (yc-identity) is clean enough for the AI code factory to pattern-match

---

## Phase 2 — Catalog and Browse (2 weeks)

A visitor can see products with prices. No cart yet; just browse.

### Scope — Services

**yc-identity (extensions)**
- Full user registration with email + password (Argon2id)
- Login endpoint
- Role: customer (visitor → customer promotion on registration)
- Publishes `identity.user.registered.v1`, `identity.user.logged_in.v1`

**yc-image**
- Direct upload endpoint
- Image variant generation (xlarge, large, medium, thumbnail; WebP + JPEG)
- S3-compatible storage (SeaweedFS in dev)
- Publishes `image.processed.v1`
- CDN URL field present but may just serve from the dev S3 bucket

**yc-product**
- Suppliers CRUD (system_admin)
- Products CRUD (shop_admin)
- SKUs CRUD (shop_admin)
- Categories CRUD (shop_admin)
- Public browse endpoint (`GET /products` with basic filtering, pagination)
- Public product detail endpoint
- Published events: `product.supplier.*`, `product.created|updated|deleted.v1`, `product.sku.*`, `product.category.*`
- `sku_search_projection` table populated on CRUD, no event-driven updates yet (added in Phase 3)
- Simple name/description full-text search via Postgres FTS

**yc-pricing**
- Base price CRUD
- Tenant pricing config (store currency, tax inclusive flag, default jurisdiction)
- Tax classes CRUD
- Tax rules CRUD
- Pricing pipeline with: BasePriceEvaluator, TaxEvaluator, DisplayCurrencyEvaluator
- Exchange rate fetching from exchangerate.host (hourly)
- Public price lookup endpoint
- Published events: `pricing.base_price.set.v1`, `pricing.config.changed.v1`

### Scope — Shared libraries (extensions)

- `yc-money` — Money type with minor units, allocation, rate application
- `yc-httpkit` — pagination helpers, OpenAPI integration via `oapi-codegen`

### Scope — Gateway extensions

- Dynamic service self-registration (services call yc-gateway on startup)
- Health polling of registered services

### Deferred

- Promotions
- Role-contextualized search (favourites, popularity)
- External search engine
- Supplier portal

### Milestone

Scripted demo:
1. System admin seeds a supplier (via API or script)
2. Shop admin creates products with variants and SKUs
3. Shop admin sets base prices and tax rules
4. Shop admin uploads product images (served from the dev S3 bucket)
5. Visitor browses `GET /products` and sees products with prices in ZAR
6. Visitor sets display currency header (or query param) and sees USD prices
7. Full pricing audit trail visible in response for a single product

### Exit criteria

- Catalog navigable
- Prices correct for both tax-inclusive and tax-exclusive products
- VAT breakdown correct
- Currency conversion correct
- Admin APIs protected by role

---

## Phase 3 — Cart and Authenticated Users (1.5 weeks)

Customers can build carts. Visitor carts merge on login. Preferences work.

### Scope — Services

**yc-preference**
- Preferences model (display currency, locale, primary channel, messaging channels)
- Public endpoints: `GET /preferences/me`, `PATCH /preferences/me`
- Default seeding on `identity.user.registered.v1`
- Published events: `preference.currency.changed.v1`, `preference.channel.*.v1`

**yc-cart**
- Carts per `(tenant, user, type)`
- Add, remove, update quantity
- Multiple cart types (shopping, wishlist)
- Price snapshotting on add (calls yc-pricing)
- Price change detection on read
- Visitor cart merge into customer cart on login
- Published events: `cart.*.v1`
- Subscribes to `identity.user.logged_in.v1` for merge trigger

**yc-product (extensions)**
- Subscribes to `cart.item.added.v1` → increments `popularity_score` (projection sync starts here)
- Subscribes to `pricing.base_price.set.v1` → updates `base_price` in projection
- Role-contextualized search:
  - Visitor sees popularity-sorted
  - Customer sees "favourites" facet (empty for now; populated after Phase 4 orders)

### Scope — Gateway extensions

- Full tenant resolution with hostname mapping table
- Rate limiting per actor and per tenant
- Login endpoint routing

### Scope — yc-identity extensions

- OAuth for Google and Facebook
- Refresh token flow
- Password reset (just emits event; yc-message will handle in Phase 7)

### Deferred

- Abandoned cart detection (Phase 7, with yc-message)
- Channel verification (Phase 7)
- Favourite recomputation (Phase 7, driven by orders)

### Milestone

1. Visitor adds items to cart (visitor token)
2. Visitor registers with email + password; cart merges to customer cart
3. Customer logs out, logs in with Google OAuth; previous cart intact
4. Customer changes display currency; cart totals re-render in new currency
5. Shop admin changes base price of an item in cart; customer's cart shows "price changed" indicator
6. Search sorts by popularity (reflecting cart adds via events)

### Exit criteria

- Multi-cart support working
- Visitor-to-customer merge works deterministically
- Price snapshotting and change detection working
- Event-driven projection in yc-product proven out

---

## Phase 4 — Checkout Happy Path (3 weeks)

The hardest phase. Full saga from cart to paid order with Stripe. Inventory reservation, pricing authority at checkout, payment authorization + capture, order creation and confirmation, cart clear. The saga calls each participant synchronously (ADR-019).

This phase will stress every piece of infrastructure. Budget for debugging.

### Scope — Services

**yc-inventory**
- Stock levels per SKU per tenant
- Tenant inventory config (enabled/disabled)
- SKU-level `inventory_tracked` honored
- Admin APIs: receive, adjust, set threshold
- Reservation lifecycle (reserve → commit/release/expire) via idempotent internal endpoints
- Expiry sweeper (30s interval)
- Published events: `inventory.*.v1`
- Subscribes to `product.sku.*.v1`

**yc-payment**
- Stripe driver (Variant A — separate authorize + capture)
- Internal endpoints: `/internal/authorize`, `/internal/capture`, `/internal/void`, `/internal/refund` — idempotent by `Idempotency-Key`; `pending` result for 3-D Secure
- Webhook endpoint: `/webhooks/stripe` with signature verification
- Encrypted credential storage per tenant
- Published events: `payment.*.v1`

**yc-orders**
- Internal create (`pending_payment`), confirm and abandon endpoints for checkout
- Order snapshot (items, pricing, taxes, addresses — all captured at creation)
- Order status lifecycle: `pending_payment`, `abandoned`, `received`, `cancelled` (shipping-driven states come in Phase 6)
- Published events: `order.placed.v1` (on confirm only), `order.cancelled.v1`
- Order number generation (per-tenant sequence)

**yc-checkout**
- Full saga orchestration (Variant A path) with synchronous internal calls (ADR-019)
- Session management with 30-minute TTL
- Session resumption on service restart
- Compensation flows for every failure point before capture; forward-only CONFIRMING after it
- Guest checkout: email + phone + address validation per ADR-010
- Published events: all `checkout.*.v1` (no command events)

### Scope — Shared library extensions

- `yc-httpkit` — internal-call client (service token, header propagation, timeouts, retries with a fixed `Idempotency-Key`) and server-side `Idempotency-Key` middleware that stores and replays responses
- `yc-authctx` — guest contact propagation through saga calls

### Scope — yc-product extensions

- Subscribes to `inventory.stock.*.v1` for `in_stock` facet in projection
- `is_digital` flag on products

### Scope — yc-cart extensions

- Subscribes to `checkout.completed.v1` to clear source cart

### Deferred

- Yoco driver (Phase 5)
- Shipping (Phase 6 — use a stub shipping evaluator for v1; set a flat shipping cost or ignore)
- Email notifications (Phase 7 — events fired but no messages sent yet)
- Analytics (Phase 8)

### Milestone — End-to-end Stripe checkout

Scripted demo:
1. Customer with cart containing 2 items (1 tracked, 1 untracked digital) initiates `POST /checkout`
2. Session enters PRICING; snapshot includes current prices, VAT
3. Customer confirms with Stripe test card token
4. Session advances RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED, and the confirm request returns the order
5. Order visible in `/orders/mine`
6. Payment visible in Stripe dashboard
7. Cart is cleared
8. Inventory decremented for the tracked item only; untracked digital item unaffected
9. Full trace visible in Jaeger across all services

Scripted failure demo:
10. Repeat but with an invalid payment token; saga enters COMPENSATING, releases reservation, no order placed (the pending order, if created, is abandoned)
11. Repeat with out-of-stock item; reservation fails fast; no payment attempted

### Exit criteria

- Happy path checkout works deterministically
- All compensation paths tested (one per failed step)
- Idempotency verified: replay a checkout confirmation; no double-charge, no duplicate order
- Session resumption verified: kill yc-checkout mid-saga, restart, saga completes
- Reservation expiry verified: stall checkout past 15 minutes, confirm compensation
- Session expiry verified: stall checkout past 30 minutes, confirm EXPIRED and compensation

---

## Phase 5 — Yoco Integration (1 week)

Add Yoco as a second gateway. This proves the gateway abstraction and the Variant B (combined charge) path.

### Scope

**yc-payment extensions**
- Yoco driver (Variant B — combined charge)
- Yoco webhook endpoint with signature verification
- Driver capability reporting (`SupportsSeparateAuthCapture() = false`)

**yc-checkout extensions**
- Variant B path: CHARGING state replaces AUTHORIZING + CAPTURING, after ORDER_CREATE (ADR-019)
- Gateway selection logic (per tenant, per currency), asked of yc-payment at session start
- Variant B compensation: the order is created before the charge, so a failed order creation moves no money; a hosted charge paid after the session ended is refunded (ADR-019 rule 12)

**yc-payment config**
- Per-tenant gateway enablement
- Currency-to-gateway routing: a priority and an optional currency list per gateway (ADR-013)

### Milestone

`task demo:phase5` runs it against the fake Stripe and Yoco (`scripts/demo-phase5.sh`). Store currency is fixed per shop, so it provisions a ZAR shop and a USD shop.

1. Tenant configured with both Stripe and Yoco; Yoco priority for ZAR
2. Checkout in ZAR routes to Yoco (Variant B); runs CHARGING instead of AUTHORIZING+CAPTURING
3. Checkout in USD routes to Stripe (Variant A)
4. Compensation test: force order creation failure on a Yoco checkout; verify no charge is made. Cancel a session with the Yoco page open, then pay it; verify the late charge is refunded
5. Yoco webhook test: simulate a delayed charge confirmation; order state updates correctly

### Exit criteria

- Both gateways work for happy path
- Variant B compensation (late-charge refund) verified
- Webhook idempotency verified (replay same webhook; no double update)
- Gateway capability reporting drives saga variant selection correctly

---

## Phase 6 — Order Fulfillment (2 weeks)

Shop admin can ship orders; customers see status. Shipping quote integrated into checkout.

### Scope — Services

**yc-shipping**
- Zones CRUD
- Rates CRUD (flat, weight-tiered, price-tiered)
- Manual carrier driver (no external carrier integration)
- Quote endpoint for yc-checkout
- Label creation (stub for manual driver — generates a shipment record and tracking number)
- Admin endpoints to manually update shipment status (`in_transit`, `delivered`, `exception`)
- Published events: `shipping.*.v1`

**yc-orders extensions**
- Full lifecycle: `received` → `packaged` → `shipped` → `delivered`
- Shop admin endpoints: `POST /admin/orders/{id}/package`
- Subscribes to `shipping.*.v1` for status transitions
- Return request workflow (customer-initiated, admin-approved; accepting refunds at once, no return shipment or restock)
- Refund via yc-payment integration, plus an admin partial refund

**yc-checkout extensions**
- Shipping step (ADR-022): `POST /checkout/{id}/shipping` takes the delivery address, quotes yc-shipping and selects an option; confirm must use the same address
- `requires_shipping` detection: skip shipping if all items are digital
- Shipping address validation per ADR-015 (format only; JSON structure)

**yc-product extensions**
- SKU weight and dimensions fields (weight drives weight-tiered quotes; dimensions are stored for future carriers)

### Milestone

`task demo:phase6` runs it against the fake Stripe (`scripts/demo-phase6.sh`). The decisions are in ADR-022.

1. Shop admin configures zones (Gauteng, Western Cape, Rest of SA) and rates
2. Customer with physical items checks out; shipping options displayed based on address
3. Customer selects standard shipping; quote included in totals
4. Order placed with shipping amount
5. Shop admin marks order as packaged
6. Shop admin manually updates to in_transit (manual driver)
7. Shop admin updates to delivered
8. Customer sees status transitions in `/orders/mine`
9. Return flow: customer requests return; admin accepts; refund issued through yc-payment
10. All-digital order: skips shipping entirely, no delivery address required

### Exit criteria

- Physical and digital orders both work
- Return and refund flow works
- Order status reflects shipping events accurately
- Delivery address stored as free-form JSON

---

## Phase 7 — Reviews, Preferences, Messaging (1 week)

Customer-facing polish: reviews, notifications, abandoned cart.

### Scope — Services

**yc-message**
- Email driver (SMTP via MailHog in dev)
- SMS driver stub (interface + fake sender; real provider later)
- Template system per `(tenant, message_type, channel, locale)`
- Platform-default templates for v1 message types
- Subscribes to triggering events: `identity.user.registered.v1`, `identity.user.password_reset_requested.v1`, `order.placed.v1`, `order.shipped.v1`, `order.delivered.v1`, `order.cancelled.v1`, `order.refunded.v1`, `cart.abandoned.v1`, `review.prompt_requested.v1`
- Message history storage
- Channel verification flow

**yc-review**
- Review CRUD per `(tenant, product, user)`
- Moderation workflow (manual by default)
- Likes
- Verified purchase check against yc-orders
- Google reviews cache (optional; stub if no Google key)
- Published events: `review.*.v1`

**yc-cart extensions**
- Abandoned cart detection (24h idle; emits `cart.abandoned.v1`)
- Subscribes to `preference.currency.changed.v1` (refresh on next read)

**yc-preference extensions**
- Channel verification endpoints
- Subscribes to `message.bounced.v1` (unverify channel)
- Subscribes to `message.verification_confirmed.v1` (mark verified)

**yc-product extensions**
- Subscribes to `review.approved.v1`, `review.like.*.v1` for projection
- Subscribes to `order.placed.v1` for favourites projection

### Milestone

1. Customer registers → welcome email in MailHog
2. Customer completes order → confirmation email with order details
3. Shop admin marks shipped → shipping notification email
4. Customer leaves product review; shop admin moderates; approved review visible on product
5. Customer likes products; appears in "favourites" facet after an order
6. Abandoned cart email fires 24h after idle cart (test by setting threshold to 1 minute)
7. Customer requests password reset → reset email delivered

### Exit criteria

- All v1 message types rendered correctly
- Channel verification flow works
- Review moderation + verified purchase check works
- Favourites facet in product search populated for authenticated customers

---

## Phase 8 — Operational Maturity (2 weeks)

Full observability, audit, analytics, and GDPR cascade.

### Scope — Services

**yc-monitor (full)**
- Health check loop for all registered services
- DLQ subscription across all tenants
- Metric rollups (1m, 5m, 1h, 1d buckets)
- Alert rules + alerts
- Alert routing to yc-message and optional webhooks
- Admin dashboard API
- Published events: `monitor.*.v1`

**yc-audit (full)**
- Subscribes to all audit-worthy events
- Immutable audit log with INSERT-only privileges
- GDPR cascade register with 72h incident threshold
- Export endpoints

**yc-analytics (full)**
- Fact and dimension tables
- Event-driven fact ingestion
- Nightly aggregate rebuild
- Dashboard query API (named queries)
- Milestone events (first order, revenue tiers)

**Cross-service**
- Every service: implement `identity.user.deleted.v1` subscriber; publish `platform.gdpr.cascade_completed.v1` on completion
- yc-identity: user deletion endpoint; pseudonym generation
- End-to-end GDPR cascade test

### Milestone

1. All services registered in yc-monitor; health dashboard shows green
2. Force a service to fail; monitor marks it unhealthy; alert fires; routes to admin email
3. Force poison messages into a service; DLQ fills; monitor shows spike; replay via `yc-dlq-cli` succeeds
4. Shop admin dashboard shows revenue, order count, top products, conversion funnel for the week
5. System admin triggers a GDPR deletion for a test customer:
   - All PII-holding services acknowledge within 72h
   - Audit register shows "complete"
   - Orders retained but with pseudonymized user_id
   - Analytics facts retained but with pseudonymized user_id
6. Chargeback simulation: force a Stripe test chargeback; `payment.chargeback_received.v1` → `order.disputed.v1` → alert to shop admin

### Exit criteria

- Full observability chain works
- GDPR cascade completes within 72h for all participating services
- GDPR incident alerting works (test by holding one service back)
- Audit log immutable (verify database privileges prevent UPDATE/DELETE)
- Analytics aggregates match ground truth (query facts, compare to aggregates)

---

## Phase 9 — Hardening (2 weeks)

Security review, load test, polish, documentation.

### Scope

- **Security review**
  - Static analysis (gosec results triaged)
  - Dependency vulnerability scan (`govulncheck`)
  - Secrets review (nothing committed; `.env.example` complete)
  - Auth boundary testing (role enforcement, tenant isolation via RLS)
  - OWASP top 10 review for public endpoints
  - Rate limit tuning
- **Load testing**
  - Browse load (visitor traffic): target 500 RPS on `/products`
  - Checkout load: target 10 concurrent sessions sustained
  - Event throughput: target 1000 events/sec on NATS
  - Identify bottlenecks; add indexes, caches, connection pool tuning
- **Chaos testing**
  - Kill each service during a checkout; verify saga recovery
  - Kill NATS; verify outbox retries catch up on recovery
  - Simulate slow dependencies; verify timeouts and compensation
- **Documentation**
  - All ADRs reviewed and current
  - All service READMEs up to date
  - Runbooks: DLQ replay, GDPR incident, tenant provisioning, Yoco setup
  - API documentation generated from OpenAPI specs
- **Deployment preparation**
  - Dockerfiles optimized (image size, layer caching)
  - Health checks tuned (readiness vs liveness distinction)
  - Config review: nothing hardcoded that should be configurable
  - Graceful shutdown verified for every service
- **Backlog grooming**
  - All deferred items from previous phases catalogued
  - Priority ranking for v2

### Milestone

- Load test report shows platform handles target load
- Security review signs off (no criticals)
- Chaos test report shows acceptable recovery times
- Documentation complete enough for a new engineer to onboard in 1 day
- Runbooks tested by someone other than the author

### Exit criteria

- Platform ready for first real tenant
- No known critical bugs
- All v1 scope delivered

---

## Cross-Cutting Workstreams

Running in parallel across all phases, not sequential:

### Testing strategy

- **Unit tests**: alongside every change. CI gates on them from Phase 1.
- **Integration tests**: per service, exercising DB + event bus with testcontainers. Added from the first service.
- **E2E tests**: full saga tests in `test/e2e/`. Added in Phase 4, maintained through all later phases.
- **Contract tests**: schema validation on every event publish/consume via yc-eventbus. Automatic.
- **Load tests**: `test/load/` with `k6` or `vegeta`. Added in Phase 9 but harness in place from Phase 4.

### Documentation discipline

Every phase ends with a documentation review:
- Service READMEs updated with new endpoints
- ADRs added if a non-obvious design decision was made
- Standards docs updated when conventions evolve
- Runbooks added when operational scenarios are encountered

### AI code factory workflow

Treat the AI code factory as a junior engineer who needs good prompts and reviews. For each service:
1. **Human writes the service requirement markdown** (already done)
2. **Human provides context**: linked ADRs, relevant standards, reference service (yc-identity)
3. **AI scaffolds** using `scripts/gen-service.sh`
4. **AI implements** endpoint by endpoint, event by event, with the human reviewing each PR
5. **Human writes the first integration test**; AI writes the rest following the pattern
6. **Human reviews security and tenant isolation** every PR — these are the AI's blind spots

### Dependency management

- Weekly `go get -u` across all modules, with CI running the full test suite
- `govulncheck` on main branch, blocking merge if criticals found
- Pin major versions; allow minor/patch floats
- Review and update `.golangci.yml` quarterly

---

## Risk Register

Risks that could derail the plan, with mitigation:

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| yc-eventbus proves harder than estimated | Medium | High | Build it in Phase 1 against yc-identity so it's exercised early, not retrofitted |
| Yoco integration behaves differently than documented | Medium | Medium | Build Stripe first (Phase 4); Yoco (Phase 5) can slip without blocking the plan |
| Checkout saga compensation has edge cases | High | High | Dedicate 3 weeks to Phase 4; test every failure mode explicitly |
| Multi-tenancy RLS bugs leak data across tenants | Medium | Critical | Write a cross-tenant isolation test suite in Phase 1; run it against every service |
| NATS JetStream operational complexity | Medium | Medium | Build the `yc-dlq-cli` tool in Phase 1; invest in NATS observability (counts per subject, lag per consumer) |
| GDPR cascade incomplete after 72h in practice | Medium | High | Test the cascade in Phase 8 before any real customer data; make the register the source of truth |
| Address JSON flexibility creates issues for shipping | Low | Medium | Define a recommended convention (ADR-015) and document which fields each carrier driver requires |
| Performance under load worse than expected | Medium | Medium | Phase 9 load testing with explicit targets; have an index-tuning and caching plan ready |

---

## Decision Points During the Build

These are decisions deferred intentionally — they can be made when we have the context to decide:

1. **Phase 2 end**: External search engine (OpenSearch/Meilisearch) or stick with Postgres FTS? Decide based on Phase 2 performance observations.
2. **Phase 4 end**: Payment driver pattern — stick with hand-rolled drivers or adopt a library like `go-stripe`? Decide after implementing Stripe.
3. **Phase 6 end**: Which real South African courier to integrate first (post-v1)? Decide based on first tenant's needs.
4. **Phase 8 end**: Analytics storage — stick with Postgres or move to ClickHouse? Decide based on query performance.
5. **Phase 9 end**: Deployment target — Kubernetes, Nomad, ECS, or Docker Compose on a VM? Decide based on tenant onboarding model.

---

## What Comes After v1

Not planned here but worth signposting so decisions in v1 don't block them:

- Saved payment methods (gateway-side only)
- Backorder support
- Multi-warehouse inventory
- Marketing campaigns via yc-message
- Shopify-style frontend admin UI (currently v1 ships API only)
- Mobile app support
- Subscription products
- Marketplace model (multiple sellers per shop)
- Multi-currency acceptance (not just display)
- Advanced tax (EU VAT MOSS, US nexus)
- Real courier integrations
- SSO for shop admins

Each of these should be its own project with its own plan.
