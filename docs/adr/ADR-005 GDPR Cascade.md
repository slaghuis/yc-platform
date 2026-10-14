# ADR-005: GDPR Cascade

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Personal data about a user is spread across many services, each with its own database (ADR-017). A deletion request must reach all of them, be provably completed, and still respect legal retention of financial records.

## Decision

Deletion of personal data is **event-driven** and **anonymizing**, not destructive.

## Trigger

`identity.user.deleted.v1` published by YC-Identity after a validated deletion request (customer-initiated or admin-initiated).

## Cascade Protocol

1. **Every PII-holding service MUST subscribe** to `identity.user.deleted.v1`
2. Current PII-holding services, each of which must report completion. This list is the `expected_services` configuration of the yc-audit cascade register; a service that starts holding user data is added to both.

   | Service | Action |
   |---|---|
   | yc-identity | Owns the trigger; pseudonym mapping kept under legal hold |
   | yc-preference | Delete preferences |
   | yc-cart | Delete carts |
   | yc-checkout | Pseudonymize sessions; purge guest contact after the operational window |
   | yc-orders | Pseudonymize (financial record — see Exceptions) |
   | yc-payment | Pseudonymize actor references (financial record — see Exceptions) |
   | yc-shipping | Pseudonymize delivery addresses after the operational window |
   | yc-review | Anonymize reviews, delete likes |
   | yc-message | Anonymize recipient addresses after the operational window |
   | yc-inventory | Delete back-in-stock subscriptions |
   | yc-product | Delete the favourites projection |
   | yc-analytics | Pseudonymize facts |

   yc-audit runs the register and is special (see below). yc-image, yc-monitor, yc-gateway and yc-pricing hold no per-user data and do not take part.
3. Within **24 hours** of receiving the event, each service must:
   - **Anonymize** the user's PII (replace with `anon-<hash>`), OR
   - **Delete** the row entirely if business rules allow
4. On completion, the service publishes `platform.gdpr.cascade_completed.v1`:
   ```json
   {
     "user_id": "...",
     "service": "yc-cart",
     "action": "anonymized|deleted",
     "record_count": 42
   }
   ```

## Tracking

- **YC-Audit** maintains a GDPR cascade register
- A cascade is **complete** only when every expected service has reported
- A cascade incomplete after **72 hours** is a **compliance incident**, alerting the system administrator and the shop administrator of the affected tenant
- The register itself is never anonymized — proof of compliance is required

## Exceptions

- **Orders and Payment**: legally required to retain transaction records. These services **pseudonymize** rather than delete — the user's identity is replaced with a one-way token, but order/payment facts remain. The link table user_id → token is retained ONLY in YC-Identity, under legal hold.
- **YC-Audit**: retains the fact that a cascade occurred and the pseudonymized user token, but no PII.

## Events on the Bus

Events already published to the bus **are not deleted**. Instead:
- Future events referencing the deleted user carry the pseudonymized token
- Historical events are consumed only by downstream services that are themselves subject to the cascade
- Retention policy (ADR-004) ensures old events age out
