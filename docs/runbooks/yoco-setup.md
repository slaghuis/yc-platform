# Setting Up Yoco for a Shop

Connect a shop to Yoco so ZAR checkouts take payment on Yoco's hosted page (ADR-013, Variant B). Run this after the tenant is provisioned (`onboarding-tenant.md`) and a shop admin can log in.

## How it works

- yc-payment keeps each shop's Yoco secret key and webhook secret, encrypted at rest (ADR-009). Neither is ever returned by the API; `api_key_hint` shows a masked key.
- A Yoco checkout is hosted: confirm answers `202` with `next_action.redirect_to_url`, the customer pays on Yoco's page and returns to the storefront's `return_url`. The outcome arrives **only by webhook** (`POST https://<shop host>/webhooks/yoco`), which resumes the saga. Without a working webhook, every Yoco checkout stays in `CHARGING` until it expires and is compensated.
- Yoco takes ZAR only. A shop selling in another currency routes to Stripe (or fails with `412` if no gateway takes the currency).

## Prerequisites

- The shop's hostname resolves to the gateway and serves **HTTPS** with a valid certificate. Yoco delivers webhooks only to a public HTTPS URL.
- The shop's Yoco account: a **secret key** from the Yoco portal (Sell Online → Payment Gateway). Use `sk_test_…` first, then `sk_live_…`.
- A shop admin access token (`TOKEN` below) on the shop's host.
- A Yoco account used by **one shop only**. Yoco sends every event of an account to every webhook the account has registered, so two shops sharing an account each receive the other's payments (they fail verification or match no transaction, but it is noise and a support risk).

## Procedure

1. **Register the webhook with Yoco.** Yoco returns the signing secret once; keep it for the next step and do not paste it into tickets or chat.
   ```bash
   curl -fsS -X POST https://payments.yoco.com/api/webhooks \
     -H "Authorization: Bearer $YOCO_SECRET_KEY" -H "Content-Type: application/json" \
     --data '{"name":"yc-shop-abc","url":"https://shop-abc.com/webhooks/yoco"}' | jq -r .secret
   ```
   The secret starts with `whsec_`. `GET /api/webhooks` lists the account's webhooks; delete stale ones (`DELETE /api/webhooks/{id}`) so payments are not delivered to an old host.
2. **Store the credentials** in yc-payment. `priority` orders gateways that take the same currency (lowest first, default 100); give Yoco the lower number to make it the ZAR default.
   ```bash
   curl -fsS -X PUT https://shop-abc.com/admin/gateway-config/yoco \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     --data "{\"enabled\":true,\"api_key\":\"$YOCO_SECRET_KEY\",\"webhook_secret\":\"$YOCO_WEBHOOK_SECRET\",\"priority\":10}"
   ```
   An omitted `api_key` or `webhook_secret` keeps the stored one, so either can be rotated alone. An empty `webhook_secret` removes it, which makes webhooks fail.
3. **Test the key.** `ok: false` carries Yoco's reason (usually a wrong or revoked key).
   ```bash
   curl -fsS -X POST https://shop-abc.com/admin/gateway-config/yoco/test -H "Authorization: Bearer $TOKEN"
   ```
4. **Check routing.** `GET /admin/gateway-config` should list Yoco as `enabled`, `webhook_secret_set: true` and with the intended priority. A storefront checkout in ZAR then shows `payment: {"gateway": "yoco", "flow": "charge", "hosted": true}` on the session.
5. **Pay a test order** end to end with the `sk_test_` key: start a checkout, confirm with a `return_url`, pay on Yoco's page with a Yoco test card, and check the session reaches `COMPLETED` and the order `received`. If it stays in `CHARGING`, see Troubleshooting.
6. **Go live:** repeat steps 1–3 with the `sk_live_` key: register the webhook with the live key, store the key with the secret that registration returns, and delete the test webhook.

## Troubleshooting

| Symptom | Check |
|---|---|
| Sessions stay in `CHARGING` after the customer paid | The webhook is not arriving or not verifying. yc-payment logs a verification failure and publishes `payment.webhook_signature_failed.v1` (yc-monitor and yc-audit show it). Check the URL registered with Yoco, that the gateway routes `/webhooks/yoco` for the host, and that the stored secret belongs to that webhook. |
| `401` on `/webhooks/yoco` | Wrong `webhook_secret`, or the server clock is more than 3 minutes out (Standard Webhooks tolerance). |
| Checkout start or confirm fails with `412` | No enabled gateway takes the store currency. Check `enabled` and `currencies` in `GET /admin/gateway-config`. |
| `400` setting `currencies` | Yoco takes ZAR only. |
| Customer paid after the session expired | yc-checkout refunds a late charge in full (ADR-019 rule 12) and records `late_charge_refunded` in the session's compensation log. |

## Limitations

- Yoco disputes are not handled; only Stripe chargebacks reach yc-orders (ADR-025 rule 17). Handle Yoco disputes in the Yoco portal.
- The Yoco API cannot cancel a hosted checkout, so a compensated session's Yoco page stays payable until Yoco expires it; a late payment is refunded automatically.
- Refunds go through yc-orders' admin cancel, not the Yoco portal, so the order and transaction stay consistent.

## Related

- `docs/services/yc-payment.md` (Yoco driver, webhooks, routing)
- `docs/services/yc-checkout.md` (Variant B, pending payments, late charges)
- `scripts/demo-phase5.sh` (the same steps against the fake Yoco or Yoco's test environment)
- `checkout-stuck-session.md`
