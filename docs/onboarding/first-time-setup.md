# First-Time Setup

Install the tools in [`developer-setup.md`](developer-setup.md) first.

```bash
git clone https://github.com/slaghuis/yc-platform.git
cd yc-platform

task bootstrap        # installs hooks, creates .env, generates dev keys
task devstack:up      # starts Postgres, NATS, S3 (SeaweedFS), Jaeger, MailHog
task test             # verify everything compiles and tests pass
```

## Local Services

Once the dev stack is up:

| Service | URL |
|---|---|
| Postgres | `postgres://postgres:devpass@localhost:5432` |
| NATS | `nats://localhost:4222` |
| NATS admin | http://localhost:8222 |
| S3 API (SeaweedFS) | http://localhost:9000 (devkey / devsecret, bucket `yc-images`) |
| S3 admin UI | http://localhost:9001 |
| Jaeger | http://localhost:16686 |
| MailHog | http://localhost:8025 |

## Application Services

`task devstack:app` builds and starts the application services in Docker (compose profile `app`). `task demo:phase1` runs the Phase 1 milestone demo and the outbox and DLQ drills (`scripts/demo-phase1.sh milestone|outbox|dlq` runs one part); `task demo:phase2` runs the catalog-and-browse milestone through the gateway; `task demo:phase3` runs the cart milestone (visitor cart, registration, fake Google login and merge, display currency, price change, popularity search, rate limits); `task demo:phase4` runs the checkout milestone (a paid order, a declined card, out of stock, guest checkout, 3-D Secure, and killing yc-checkout mid-saga); `task demo:phase5` runs the Yoco milestone (a ZAR shop routed to Yoco and a USD shop to Stripe, a hosted Yoco payment confirmed by webhook, webhook redelivery, an order failure that charges nothing, a late payment refunded, and switching gateway at confirm); `task demo:phase6` runs the fulfilment milestone (shipping zones and rates, options by address, packaging, tracking, delivery, a return refunded, and a digital order that skips shipping); `task demo:phase7` runs the reviews and messaging milestone (welcome, verification, order, shipping and abandoned-cart emails in MailHog, a moderated review with a shop response, likes, favourites, rating sort and a password reset by email; it restarts yc-cart and yc-review with short thresholds and restores them at the end); `task demo:phase8` runs the operational maturity milestone (every service healthy in yc-monitor, a paused service alerting by email, the shop's analytics dashboard and a first-order milestone, a chargeback disputing an order, dead letters replayed with `yc-dlq-cli`, a GDPR deletion completing in yc-audit's register, audit immutability and the analytics consistency check; it restarts yc-monitor, yc-message, yc-analytics and yc-payment with short intervals, and `docker compose --profile app up -d` restores them). `task test:e2e` runs the GDPR cascade end to end against the running stack. Product, pricing, image, preference, cart, inventory, payment, orders, checkout, shipping, message, review, monitor, audit and analytics register their routes with the gateway on start-up, so give them a few seconds after `readyz`.

**Payments.** With `STRIPE_API_KEY` (a `sk_test_` key) in `.env`, the Phase 4 demo pays through Stripe test mode and the payments appear in the Stripe dashboard. Without it, the demo runs the fake Stripe from `services/yc-payment/cmd/fake-stripe` on the host (port 12111) and points yc-payment at it. Either way it recreates the yc-payment container with the matching `YC_PAYMENT_STRIPE_API_BASE`. To receive Stripe webhooks locally, run `stripe listen --forward-to localhost:8090/webhooks/stripe -H "Host: demo-shop.localhost"` and store the `whsec_` secret it prints with `PUT /admin/gateway-config/stripe` (the demo does this when `STRIPE_WEBHOOK_SECRET` is set).

**Yoco.** The Phase 5 demo runs the fake Yoco from `services/yc-payment/cmd/fake-yoco` on the host (port 12112): it serves the Checkout API, a `_fake` endpoint to pay or fail a checkout, and signs webhooks to the shop host through the gateway. To use Yoco test mode instead, set `YOCO_API_KEY` (a `sk_test_` key) and `YOCO_WEBHOOK_URL`, a public HTTPS URL (a tunnel) that forwards to `localhost:8090/webhooks/yoco` with the ZAR shop's host; Yoco delivers webhooks nowhere else.

| Service | URL |
|---|---|
| yc-gateway | http://localhost:8090 — send `Host: demo-shop.localhost` after a demo provisions `demo-shop`, or `Host: admin.localhost` (the platform host: system admins only) |
| yc-identity | http://localhost:8081 |
| yc-monitor | http://localhost:8082 — `/sysadmin/*` needs `X-YC-Tenant-ID: _platform` and `X-YC-Actor-Type: system_admin` headers; health checks, alerts (emailed to `YC_MESSAGE_SYSTEM_ADMIN_EMAILS` by yc-message) and dead letters (`yc-dlq-cli`, see `docs/runbooks/dlq-replay.md`) |
| yc-product | http://localhost:8083 |
| yc-pricing | http://localhost:8084 — exchange rates come from api.frankfurter.dev, so display currencies need internet access |
| yc-image | http://localhost:8085 — image URLs point at the S3 bucket on http://localhost:9000 |
| yc-preference | http://localhost:8086 |
| yc-cart | http://localhost:8087 — prices carts through the gateway's internal listener (`YC_CART_SERVICE_SECRET`) |
| yc-inventory | http://localhost:8088 |
| yc-payment | http://localhost:8089 — needs `YC_PAYMENT_CIPHER_KEY` (base64 of 32 bytes); Stripe or the fake, see below |
| yc-orders | http://localhost:8092 — books shipments with yc-shipping and refunds through yc-payment (`YC_ORDERS_SERVICE_SECRET`) |
| yc-checkout | http://localhost:8093 — calls cart, product, pricing, shipping, inventory, payment and orders (`YC_CHECKOUT_SERVICE_SECRET`) |
| yc-shipping | http://localhost:8094 — zones and rates per shop; a physical checkout needs a zone that covers the address |
| yc-message | http://localhost:8095 — emails go to MailHog (http://localhost:8025); fake SMS arrive there as `<number>@sms.test`; links use `YC_MESSAGE_SHOP_URL` |
| yc-review | http://localhost:8096 — checks purchases with yc-orders (`YC_REVIEW_SERVICE_SECRET`); `YC_REVIEW_PROMPT_DELAY` sets when review prompts go out |
| yc-audit | http://localhost:8097 — insert-only audit records and the GDPR register (system admins) |
| yc-analytics | http://localhost:8098 — shop dashboards from event facts; `YC_ANALYTICS_REFRESH_INTERVAL` sets how soon aggregates catch up |

The first system admin is created with `docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD=… yc-identity /app/service bootstrap-admin --email you@example.com` (or `task run SERVICE=yc-identity -- bootstrap-admin …` locally); log in on `admin.localhost`. Shop admins register on their shop as customers and a system admin promotes them (`POST /admin/users/{id}/promote` on the admin host).

`task run SERVICE=…` uses the same host ports (set in `.env`). Stop the containers first, or they hold the ports.

## Troubleshooting

**Port conflicts.** If port 5432 (or any other) is in use, copy `docker-compose.override.yml.example` to `docker-compose.override.yml` (gitignored) and remap the port there; the example shows how.

**`database "yc_gateway" does not exist`.** `postgres-init.sh` only runs on an empty volume, so a volume created before a service's database was added lacks it. Run `task devstack:reset`, or create just that database with the statements from `deploy/compose/postgres-init.sh`.

**yc-payment exits with `cipher key is not base64`.** A `.env` created before Phase 4 has the old raw `YC_PAYMENT_CIPHER_KEY`. Copy the current value from `.env.example`.

**Dev stack won't start.**

```bash
task devstack:reset   # wipes volumes and restarts
```

**Pre-commit failures.**

```bash
pre-commit run --all-files   # see what's failing
```

**Go workspace issues.** The repo is a Go workspace (`go.work`) with one module per lib, service and tool, and no root `go.mod`.
- Relative wildcards that span modules fail from the root (`go test ./libs/...` reports `directory prefix . does not contain modules listed in go.work`). Use `task test`, run from inside a module directory, or use the module path (`go test github.com/slaghuis/yc-platform/libs/yc-eventbus/...`).
- After adding a module, add it to `go.work` (`go work use ./services/yc-foo`).
- If dependency versions disagree between modules, run `go work sync`.
