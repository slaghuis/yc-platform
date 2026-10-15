# Runbook: Checkout Session Stuck

**When to use:** yc-checkout logs `checkout session stuck; needs attention` with `alert=critical`, or a shop reports a checkout that neither completed nor failed.

Background: the saga and its compensation are in ADR-019 and [`docs/services/yc-checkout.md`](../services/yc-checkout.md). yc-checkout retries a failing step with backoff (5 s doubling to 5 min) and logs the critical alert after `saga.alert_after` attempts (default 6). Retrying is safe: every call repeats the same `Idempotency-Key`, so a participant returns its stored result rather than acting twice.

## 1. Find the session

The alert carries `session_id`, `state`, `attempts` and the last error. As a shop admin on the shop's host:

```bash
curl -s -H "Host: <shop host>" -H "Authorization: Bearer <shop admin token>" \
  http://<gateway>/admin/sessions/<session_id> | jq '{state, attempts, next_attempt_at, failure, compensation_log, history: [.history[] | {event_type, from_state, to_state, created_at}]}'
```

`GET /admin/sessions?state=CONFIRMING` (or `COMPENSATING`, `CAPTURING`) lists others in the same state. The logs and the trace for the session: `docker logs yc-checkout 2>&1 | grep <session_id>`, then open the `trace_id` in Jaeger.

## 2. Act by state

| State | What it means | What to do |
|---|---|---|
| `CONFIRMING` | The payment is **captured**. Confirming the order (yc-orders) or committing the reservation (yc-inventory) keeps failing. This state is never compensated. | Fix the participant named in the error (`docker logs yc-orders` / `yc-inventory`, check `/readyz`). The next retry completes the session; no other action. Do not refund: once confirmed, the customer has a valid order. |
| `CAPTURING` | Authorised, order pending, capture outcome unknown or yc-payment unreachable. | Check the transaction: `GET /admin/transactions?session_id=<id>` and, for Stripe, the PaymentIntent in the dashboard. Fix yc-payment or the gateway. If the session expires first, compensation voids the authorisation; a void that finds the payment captured moves the session forward to `CONFIRMING` instead. |
| `COMPENSATING` | Undoing steps (void, abandon, release) keeps failing. | `compensation_log` shows which undo succeeded. Fix the participant still failing; the next retry continues from there. |
| `AUTHORIZING`, payment `pending` | The customer has not finished 3-D Secure. | Nothing: a webhook, the customer confirming again, or session expiry (30 min) settles it. |
| `RESERVING`, `ORDER_CREATE` | A participant timed out; the sweeper retries the step. | Fix the participant. Persistent rejections compensate on their own. |

## 3. Leases

One replica drives a session at a time (`lease_owner`, `lease_until` in `checkout_sessions`). A replica that crashed holds its leases until `saga.lease_ttl` (2 min) passes; a replica restarted with the same host name frees them at start-up. A session whose lease keeps being renewed while nothing progresses points at a replica that is alive but stuck: restart it.

## 4. Don't

- Don't edit `checkout_sessions` rows by hand; the saga state and the participants' state must agree. A manual force-compensate endpoint (`POST /admin/sessions/{id}/force-compensate`) is planned but not built.
- Don't refund a captured payment of a session in `CONFIRMING`; fix the participant and let it confirm.
