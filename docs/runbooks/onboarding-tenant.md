# Onboarding a New Tenant

*Partial. `yc-tenant-cli provision` exists (Phase 1) and maps the tenant's hostnames at yc-gateway; the other services' provisioning handlers, the admin invite and `verify` land in Phase 2.*

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
3. The initial shop admin receives a password-set link via yc-message.
4. Configure gateway credentials: the shop admin logs in and sets Yoco (and optionally Stripe) credentials via the yc-payment admin API.
5. The shop admin configures:
   - Shipping zones and rates
   - Tax classes (platform defaults can be inherited)
   - Cart types if non-default
6. Verify readiness:
   ```bash
   yc-tenant-cli verify --tenant-id=shop-abc
   ```
