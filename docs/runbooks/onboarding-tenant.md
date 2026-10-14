# Onboarding a New Tenant

*Stub. Full implementation lands with yc-tenant-cli in Phase 2.*

## Prerequisites

- Tenant ID assigned: a lowercase slug of letters, digits, and single hyphens, at most 63 characters (`^[a-z0-9]+(-[a-z0-9]+)*$`, e.g. `shop-abc`). `yc-eventbus` rejects anything else, including `_platform`-like names (ADR-001).
- Tenant hostname configured in DNS
- Shop admin email captured

## Procedure

1. Provision via CLI:
   ```bash
   yc-tenant-cli provision \
     --tenant-id=shop-abc \
     --name="Shop ABC" \
     --store-currency=ZAR \
     --default-jurisdiction=ZA \
     --tax-inclusive=true \
     --primary-admin-email=admin@shop-abc.com
   ```
2. The CLI publishes `platform.tenant.provisioned.v1`; every service with a tenant-provisioning handler sets up default records.
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
