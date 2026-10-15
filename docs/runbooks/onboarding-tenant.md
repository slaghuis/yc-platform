# Onboarding a New Tenant

*Partial. `yc-tenant-cli provision` maps the tenant's hostnames at yc-gateway (Phase 1) and creates the shop's pricing config (Phase 2). The admin invite (step 3), payment credentials and `verify` are not built yet: shop admins are created by promotion (step 3).*

## Prerequisites

- Tenant ID assigned: a lowercase slug of letters, digits, and single hyphens, at most 63 characters (`^[a-z0-9]+(-[a-z0-9]+)*$`, e.g. `shop-abc`). `yc-eventbus` rejects anything else, including `_platform`-like names (ADR-001).
- Tenant hostname configured in DNS
- Shop admin email captured

## Procedure

1. Provision via CLI:
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
2. The CLI publishes `platform.tenant.provisioned.v1`; every service with a tenant-provisioning handler sets up default records. yc-gateway records the hostnames in `tenant_hosts` (ADR-020); a hostname already owned by another tenant dead-letters the event (check `/sysadmin/dlq` on yc-monitor).
3. The initial shop admin receives a password-set link via yc-message. *Until yc-message exists:* the shop admin registers on the shop's hostname (`POST /register`), and a system admin, logged in on the platform host, finds the user (`GET /admin/users?tenant_id=shop-abc&email=…`) and promotes them (`POST /admin/users/{id}/promote {"role": "shop_admin"}`). The shop admin then logs in again; tokens issued before the promotion stop working.
4. Configure gateway credentials: the shop admin logs in and sets Yoco (and optionally Stripe) credentials via the yc-payment admin API.
5. The shop admin configures:
   - Shipping zones and rates
   - Tax classes (`PUT /admin/tax-classes/{class_id}`; no platform defaults yet) and, if needed, the pricing config (`PUT /admin/pricing-config`; the currency is fixed once prices exist)
   - Cart types if non-default
6. Verify readiness:
   ```bash
   yc-tenant-cli verify --tenant-id=shop-abc
   ```
