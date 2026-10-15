# v2 Backlog

Work deliberately left out of v1, collected in Phase 9 from the phase write-ups (`implementation-plan.md`, "As built"), the service docs, the ADRs and the Phase 9 reports. Ranked by when it is needed, not by size. Product features with no v1 commitment (bundles, MFA, carrier integrations…) stay in each service doc's "Open Questions / Future Scope" and in `implementation-plan.md` ("What Comes After v1").

## P1: with the first tenants

Operational gaps an operator will hit early, and accepted security findings.

| Item | Why | Source |
|---|---|---|
| Admin invite email at provisioning | Shop admins are created by register-then-promote, which needs a system admin each time | `runbooks/onboarding-tenant.md` |
| `yc-tenant-cli verify` | Readiness of a new shop is a manual checklist | `runbooks/onboarding-tenant.md` |
| `POST /admin/sessions/{id}/force-compensate` | The stuck-session runbook has no safe action short of waiting; hand edits break saga consistency | `runbooks/checkout-stuck-session.md` |
| Service token for route registration | Anyone on the private network can register routes; today a deployment requirement | Security review finding 7, ADR-026 rule 7 |
| Delete durable consumers when a subscription is removed, and a `removing-a-service.md` runbook | A removed subscription leaves its consumer behind, pending forever and looking like lag (`yc-product-cart-item-added`, 5,388 pending in the chaos run) | Chaos report |
| `503` for a deregistered service's public routes | While a service is down its public paths return `404`, which clients don't retry; the internal listener already returns `503` | Chaos report finding 2, ADR-025 rule 18 |
| `pipeline-lib` on a current Dagger, and govulncheck over `pipeline/` in CI | Dagger v0.14 pins OpenTelemetry exporters with unreachable advisories; the CI module was outside every scan until Phase 9 | Security review finding 10 |
| `pipeline.yaml` deploy section | It names Kubernetes manifests (`deploy/k8s`) and contexts that don't exist; replace once the target is chosen | ADR-026 rule 1 |
| `rotate-secrets.md` runbook | JWT keys, service secrets, payment encryption key and gateway credentials have no written rotation procedure | `runbooks/README.md` |
| Yoco disputes | Only Stripe chargebacks reach yc-orders; Yoco disputes are handled in Yoco's portal | ADR-025 rule 17, `runbooks/yoco-setup.md` |
| Record idempotency in the business transaction | A crash after the commit but before the response is stored leaves the key "in progress": retries get 409 until the 2-minute lease lapses (seen in the kill-orders drill; the saga recovers on its own). Consumers write their dedup row in a second commit, doubling commits per event | Chaos report, load report |

## P2: before scaling out or onboarding many shops

| Item | Why | Source |
|---|---|---|
| Per-instance gateway registrations | Registrations are keyed by service name, so replicas would replace each other and one stopping would deregister all | ADR-026 rule 5 |
| Shared rate limiter and token cache (Redis) | Limits and the 60 s token cache are per gateway instance | Security review finding 8, `services/yc-gateway.md` |
| PgBouncer in transaction mode | Connection count grows with replicas × pool size | Load report, ADR-026 rule 8 |
| Batch or append-only aggregate dirty marks | Every visitor of a shop on a day updates one `agg_dirty` row | Load report |
| Tenant lifecycle subscriptions (`platform.tenant.*`) in yc-checkout, yc-payment, yc-inventory and yc-cart | Configuration changes and tenant deletion are not propagated | Service docs ("Not yet") |
| Per-shop checkout configuration (`tenant_checkout_config`, TTLs) | Session and reservation TTLs are platform-wide | `services/yc-checkout.md` |
| `POST /admin/transactions/{id}/refund` | Refunds go only through yc-orders' admin cancel | `services/yc-payment.md` |
| yc-image: delete images with their owner, bulk delete, per-shop quotas | Deleted products leave images in object storage | `services/yc-image.md` |
| Clients poll the session after a failed confirm | A confirm that fails mid-saga (502 while a service restarts) may still complete in the background; storefronts must re-read the session rather than show a failure | Chaos report |

## P3: product depth

| Item | Source |
|---|---|
| Return shipments and restocking | ADR-022 |
| Back-in-stock subscriptions (and yc-inventory joining the GDPR cascade) and `/internal/stock/batch` | `services/yc-inventory.md` |
| Analytics cohorts, promotions and lifetime value | ADR-025 rule 16 |
| Review images, helpfulness votes, abuse reports, Google reviews | ADR-024 rule 7 |
| Payment encryption key from a KMS | `services/yc-payment.md` |
| CI runs only affected modules | ADR-018 |
| Saved payment methods, backorders, multi-warehouse, real couriers, multi-currency acceptance | `implementation-plan.md` ("What Comes After v1") |
