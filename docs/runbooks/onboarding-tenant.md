# Onboarding a New Tenant

Provision a shop, give it a shop admin and connect its payments. Allow about 30 minutes, most of it waiting for DNS and TLS.

*Not built yet (v2 backlog): an emailed admin invite (the shop admin is created by promotion, step 3) and a `yc-tenant-cli verify` command (step 6 is a manual checklist).*

## Prerequisites

- A tenant ID: a lowercase slug of letters, digits and single hyphens, at most 63 characters (`^[a-z0-9]+(-[a-z0-9]+)*$`, e.g. `shop-abc`). `yc-eventbus` rejects anything else, including `_platform`-like names (ADR-001).
- The shop's hostnames in DNS, pointing at the gateway, with TLS certificates. Webhooks (Yoco, Stripe) need public HTTPS.
- The shop admin's email address.
- Access to NATS (`NATS_URL`) from where you run the CLI, and a system admin login on the platform host (`yc-identity bootstrap-admin`, `docs/services/yc-identity.md`).

## Procedure

1. **Provision** with the CLI:
   ```bash
   NATS_URL=nats://... go run ./tools/yc-tenant-cli provision \
     --tenant-id=shop-abc \
     --name="Shop ABC" \
     --hostname=shop-abc.com --hostname=www.shop-abc.com \
     --store-currency=ZAR \
     --default-jurisdiction=ZA \
     --tax-inclusive=true \
     --primary-admin-email=admin@shop-abc.com
   ```
   Currency, jurisdiction and tax-inclusive default to `ZAR`, `ZA` and `true`.
2. **Check it landed.** The CLI publishes `platform.tenant.provisioned.v1`. yc-gateway maps the hostnames (`tenant_hosts`, ADR-020); yc-pricing creates the pricing config; yc-inventory, yc-preference and yc-analytics create their per-shop defaults. Within a few seconds `curl -H 'Host: shop-abc.com' https://<gateway>/me` answers `200` (a visitor). A hostname already owned by another tenant dead-letters the event: check `GET /sysadmin/dlq` on yc-monitor and `dlq-replay.md`.
3. **Create the shop admin.** The shop admin registers on the shop's hostname (`POST /register`). A system admin, logged in on the platform host, finds the user (`GET /admin/users?tenant_id=shop-abc&email=…`) and promotes them (`POST /admin/users/{id}/promote {"role": "shop_admin"}`). The shop admin then logs in again; tokens issued before the promotion stop working.
4. **Connect payments:** `yoco-setup.md` for Yoco (ZAR). For Stripe, `PUT /admin/gateway-config/stripe` with the secret key and the webhook secret of a Stripe webhook pointing at `https://<shop host>/webhooks/stripe`, then `POST /admin/gateway-config/stripe/test`.
5. **The shop admin configures:**
   - Tax classes (`PUT /admin/tax-classes/{class_id}`; there are no platform defaults) and, if needed, the pricing config (`PUT /admin/pricing-config`; the currency is fixed once prices exist).
   - Shipping zones and rates (`POST /admin/zones`, then rates per zone).
   - Inventory (`PUT /admin/inventory/config` to turn tracking on) and cart types if non-default.
   - Products, SKUs and prices.
6. **Verify** before announcing the shop:
   - `GET /admin/gateway-config` shows the intended gateway enabled, with `webhook_secret_set: true`.
   - A test checkout (test-mode keys) reaches `COMPLETED`, and the order shows in `GET /admin/orders` as `received`.
   - The order confirmation email arrives (yc-message).
   - yc-monitor shows no open alerts for the tenant.

## Related

- `docs/services/yc-gateway.md` (hosts, routing), `docs/services/yc-identity.md` (roles, promotion)
- `scripts/demo-phase5.sh` (steps 1–5 scripted against the dev stack)
- `yoco-setup.md`, `dlq-replay.md`
