# ADR-025: Operational Maturity

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Phase 8 completes yc-monitor, yc-audit and yc-analytics, carries the GDPR cascade (ADR-005) from deletion request to proof of completion, and turns Stripe disputes into order state. The service specs describe what each service offers but leave open: how the monitor finds the services it watches and when one counts as down; which alerts exist, who receives them and how; what the audit log records and how "immutable" is enforced; how a deletion is requested, refused and proven complete; how analytics stays correct under at-least-once, out-of-order delivery; and how an operator replays dead letters. These cut across most services, so they are recorded together.

## Decision

**yc-monitor watches every service the gateway knows and raises alerts from health checks, event counts and incident events; yc-message delivers them by email. yc-audit keeps an insert-only record of sensitive events and runs the GDPR cascade register. yc-analytics keeps facts from events and derives aggregates it can rebuild and check. A deletion is refused while the user has unfinished orders, otherwise anonymises the account and publishes a random UUID pseudonym that every participant applies.**

## Rules

### GDPR cascade (amends ADR-005)

1. **Requests.** A customer deletes their account with `DELETE /me`; a system admin deletes a customer or shop admin with `DELETE /admin/users/{id}` (system admins cannot be deleted: 409). While the user has an unfinished order or open return (yc-orders `GET /internal/customer-orders?user_id=`, ADR-021) the request is 409; when yc-orders cannot answer it is 503. A disputed order waits on the bank, not the customer, and does not block.
2. **Trigger.** In one transaction yc-identity anonymises the user, unlinks OAuth identities, revokes sessions and reset tokens, records a **random UUID pseudonym** (the only link from user to pseudonym, under legal hold) and publishes `identity.user.deleted.v1` and its own `platform.gdpr.cascade_completed.v1`.
3. **Participants** subscribe through the shared `events.OnUserDeleted` helper and always report, even with nothing to change: yc-identity, yc-preference, yc-cart, yc-product, yc-review, yc-message, yc-checkout, yc-orders, yc-payment, yc-shipping and yc-analytics. Financial records (orders, payments, analytics facts) are pseudonymised: the user ID is replaced by the pseudonym. yc-payment holds no customer IDs (transactions name sessions and orders); it pseudonymises the shop admin recorded on refunds. yc-inventory takes no part until back-in-stock subscriptions exist. A participant that needs another service's data asks over an internal call (yc-shipping asks yc-orders for the customer's orders, so it now has a service secret).
4. **Register.** yc-audit opens an entry on `identity.user.deleted.v1` with the configured `expected_services` copied into it and a deadline (72 hours). Reports may arrive before the deletion and are kept until it does. When every expected service has reported, the entry is complete and yc-audit publishes `audit.gdpr.cascade_complete.v1`. A sweeper marks entries past their deadline as incidents and publishes `audit.gdpr.cascade_incident.v1`, which the monitor raises as a critical alert to the system admins. The shop admin is not alerted: only the platform can fix a service that did not report.

### Audit log

5. **What is recorded.** One record per event, keyed by its event ID so redelivery adds nothing, in six categories: role change (`identity.user.promoted.v1`), tenant lifecycle (`platform.tenant.provisioned.v1`), config change (`pricing.config.changed.v1`), GDPR (`identity.user.deleted.v1`, `platform.gdpr.cascade_completed.v1`, `audit.gdpr.cascade_complete.v1`, `.cascade_incident.v1`), financial (`payment.chargeback_received.v1`, `order.disputed.v1`, `order.refunded.v1`) and security (`payment.webhook_signature_failed.v1`). Each has a subject (`user:<id>`, `order:<id>` …) and a one-line summary.
6. **Insert-only.** `UPDATE`, `DELETE` and `TRUNCATE` on `audit_records` are revoked from the service's role and from `PUBLIC`, and triggers refuse them for any role, including one that grants itself the privileges back. Retention follows ADR-004 for `audit.*`.
7. **Access.** System admins list, read and export (CSV or JSONL, at most 10,000 records per export) every shop's records and read the register and its incidents. Shop admins list their own shop's records.

### Monitor

8. **Discovery and health.** The monitor learns services from `platform.service.registered.v1` and `.deregistered.v1` (ADR-020) and calls each one's `/readyz` every 15 seconds (3 s timeout). Three failures in a row mark it unhealthy (`monitor.service.unhealthy.v1`, critical alert); three successes recover it (`monitor.service.recovered.v1`, alert resolved). A service deregistered on shutdown or by hand is no longer checked; one the gateway deregistered for failing its checks stays checked, since that is the failure being reported.
9. **Metrics.** Counts derived from events (checkouts started, completed and failed, orders placed, payment failures, dead letters …) per shop and per dimension, rolled up into 1-minute, 5-minute, hourly and daily buckets with their own retention. Dead letters are stored for 90 days with who acknowledged them.
10. **Alert rules.**

    | Rule | Kind | Severity | Notifies |
    |---|---|---|---|
    | `dlq_rate`: more than 10 dead letters a minute, every shop together | threshold | warning | system admins |
    | `checkout_success_rate`: under 50% of at least 10 checkouts in 5 minutes, per shop | threshold | warning | system and shop admins |
    | `service_unhealthy` | health | critical | system admins |
    | `gdpr_cascade_incident` | incident | critical | system admins |
    | `order_disputed` | incident | warning | shop admins |
    | `webhook_signature_failed` | incident | warning | system admins |
    | `exchange_rates_stale` | incident | warning | system admins |

    Threshold rules are evaluated every 15 seconds; system admins change thresholds with `PUT /sysadmin/thresholds`, per shop for shop rules. One alert is open per shop, rule and dimensions; a repeated breach counts an occurrence. Threshold and health alerts resolve themselves when the condition clears; incident alerts resolve when acknowledged.
11. **Delivery.** The monitor publishes `monitor.alert.opened.v1`, `.resolved.v1` and `.acknowledged.v1`. yc-message emails opened alerts (`admin_alert`): system admins at the addresses configured in `YC_MESSAGE_SYSTEM_ADMIN_EMAILS`, shop admins from its projection of `identity.user.promoted.v1`. Critical alerts are also posted to an optional webhook (Slack-compatible JSON); a failed post is logged and not retried.

### Dead letters

12. **Replay.** `yc-dlq-cli` reads the DLQ stream (ADR-003) directly: `list`, `show` and `replay`, filtered by shop, domain, consuming service, consumer, event type or event ID. Replay publishes the original envelope to its original subject, marked with the dead letter's sequence, so every consumer sees it again and relies on its idempotency. Acknowledging a dead letter in the monitor is separate and records who looked at it.

### Analytics

13. **Facts.** yc-analytics stores facts from shop, visitor, cart, checkout, payment and order events in its own database, isolated per shop by row-level security. Days are UTC dates. An order counts when `order.placed.v1` arrives; cancellations, refunds and deliveries update it. One that arrives before its order is retried (up to 10 times) rather than dropped. The funnel counts only carts of the configured types (shopping by default).
14. **Aggregates.** Daily revenue, funnel, product performance (per SKU) and gateway performance are computed from the facts. A fact change marks its shop and day dirty; dirty days are rebuilt every 30 seconds in batches, the last two days are rebuilt nightly at 02:00 UTC, and a system admin can rebuild any range. A consistency check recomputes a range and lists the days whose aggregates differ from the facts.
15. **Milestones.** A shop's first order, order counts (10, 100, 1,000, 10,000) and revenue tiers per currency are recorded once each and published as `analytics.milestone.reached.v1`; yc-message emails the shop admins. Orders more than 24 hours old do not announce milestones, so replaying history is silent.
16. **Access.** Shop admins query their own shop's dashboard, revenue, top products, funnel and gateways, and export them as CSV; system admins see the platform, compare shops, rebuild and check consistency. Cohorts, promotions and lifetime value are deferred.

### Chargebacks

17. A Stripe `charge.dispute.created` webhook is recorded by yc-payment once per dispute and published as `payment.chargeback_received.v1`. yc-orders moves the paid order to `disputed` and publishes `order.disputed.v1`, which the monitor raises as a shop alert (emailed to the shop admins) and yc-audit records as financial. Dispute outcomes are not tracked yet.

### Internal routing

18. On the gateway's internal listener, a path that no registered service serves is **503 `unavailable`**, not 404: the owning service is down or has not registered yet, and callers must retry rather than read it as a missing resource. The public listener keeps 404.

## Consequences

**Positive**
- A failed service, a dead-letter spike, a dispute or a missed GDPR deadline reaches a person by email within a minute, without anyone watching a dashboard.
- Deletion is provable: the register names every service that reported, and an entry that is not complete by the deadline becomes an incident.
- Audit records cannot be changed by the service, by a compromised service credential or by a misdirected migration that only has the service's privileges.
- Aggregates can always be rebuilt from facts and checked against them, so a bug or a lost event is detectable and repairable.
- Dead letters can be replayed with one command once their cause is fixed.

**Negative**
- Every participant must report every deletion, even with nothing to change; adding a service that holds user data means updating `expected_services` too.
- Deleting is blocked while orders are unfinished, which can take days for a slow delivery.
- System admin email addresses are configuration, not users, so they are changed by redeploying yc-message.
- A 503 for an unknown internal path hides typos in internal URLs behind retries; logs carry the method and path.
- Facts duplicate order and payment data in yc-analytics, and their pseudonymisation is one more step in the cascade.

## Alternatives Considered

- **Monitor reads the gateway's registry over HTTP:** rejected. The registration events already exist, and the monitor keeps watching a service the gateway dropped.
- **Alert by webhook only:** rejected. Email works for every operator and shop admin without setup; the webhook is an addition for critical alerts.
- **Immutability by application code only:** rejected. A bug or a stolen credential could still change records; privileges and triggers are enforced by the database.
- **Hash-derived pseudonyms:** rejected. A hash of the user ID can be recomputed by anyone who has the ID; a random UUID is linked only by yc-identity's legal-hold table.
- **Analytics computed on the fly from facts:** rejected for dashboards. Daily aggregates keep queries cheap; the facts remain the source of truth for rebuilds and consistency checks.
- **404 for unknown internal paths:** rejected. Callers treat 404 as "no such resource" (yc-message skips a customer it cannot find), so a restarting service would silently drop work.
