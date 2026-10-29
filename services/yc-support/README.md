# yc-support

Customer support contact threads: a customer or visitor opens a request and
gets a reference; the shop and the customer exchange messages on the thread
until a shop admin closes it or it has been idle for 72 hours.

Feature spec: `docs/features/Customer support contact threads.md`.

## Behaviour

| Status | Meaning |
|---|---|
| `new` | Opened; no shop message yet |
| `open_customer` | The shop answered; the requester acts next |
| `open_shop` | The requester replied; the shop acts next |
| `closed` | Final. No reopen; any post or a second close is 409 |

- Shop reply: `new` / `open_shop` / `open_customer` → `open_customer`.
  Customer reply: `open_customer` → `open_shop` (`new` stays `new` until the
  shop has answered).
- Close: shop admin (`closed_reason=staff`) or the sweeper (`idle`) when
  `last_activity_at` is older than `support.idle_ttl` (72h). The sweeper runs
  every `support.sweep_interval` (1m), posts a `system` message, and notifies.
- Visitors can only create. They answer by email until they register with the
  same address and `POST /internal/support/requests/claim` attaches their
  requests (closed ones too) to the new user.
- Topics: `allergen`, `delivery`, `order`, `payment`, `feedback`, `other`.
- References look like `SUP-7K3M9Q`, unique per shop.

## APIs

OpenAPI: `api/openapi.yaml` (`task gen:openapi` regenerates `api/generated`).

| Audience | Routes |
|---|---|
| Customer / visitor | `POST /support/requests` (both), `GET /support/requests`, `GET /support/requests/{id}`, `POST /support/requests/{id}/messages` (customers) |
| Shop admin | `GET /admin/support/requests` (filter `status`, `topic`, `email`, `reference`), `GET /admin/support/requests/{id}`, `POST …/messages`, `POST …/close` |
| Service | `POST /internal/support/requests/claim` `{user_id, email}` |

## Notifications

Sent after each committed change with `POST /internal/send` on yc-message;
a failure is logged and never fails the request.

| yc-message type | To | When |
|---|---|---|
| `support_ack` | requester | request opened |
| `support_new_request` | shop | request opened |
| `support_shop_reply` | requester | shop responded |
| `support_customer_reply` | shop | requester replied |
| `support_closed` | requester **and** shop | closed (context `audience` is `customer` or `shop`; `closed_reason` is `staff` or `idle`) |

Requesters with an account are addressed by `user_id` (yc-message resolves the
channel, ADR-023); visitors by email at `contact_email`. The shop is emailed at
yc-tenant's `contact_email` (cached for `tenant.contact_cache_ttl`).

Template context: `audience`, `reference`, `topic`, `request_id`,
`contact_name`, plus `excerpt` (messages), `closed_reason` (closes) and
`support_url` (accounts only: `/account/support/{id}`, prefixed by
`support.storefront_base`, in which `{tenant_id}` is replaced).

The idempotency reference of each message is
`support:<request_id>:<created|message:<id>|closed>:<customer|shop>`.

## Events

Published through the outbox on the `YC_SUPPORT` stream (1-year retention):
`support.request.created.v1`, `support.message.posted.v1`,
`support.request.closed.v1`. Payloads hold IDs, the reference and statuses
only. Schemas: `schemas/yc-event-schemas/support/`.

## GDPR

On `identity.user.deleted.v1`, the user's requests lose `contact_name`,
`contact_email`, `contact_phone`, `page_url` and `user_id` and get
`redacted_at`; their message bodies become `[removed]`; a deleted shop admin's
messages lose the author. The service then publishes
`platform.gdpr.cascade_completed.v1` (`anonymized`). The shop keeps the
reference, topic, status and timestamps.

## Retention

Threads are kept until the requester is erased. A retention window for closed
threads is not yet configured.

## Local development

```bash
task run SERVICE=yc-support
task test                # unit tests
task test:integration    # needs Docker (Postgres and NATS containers)
```

Database: `yc_support` (`YC_SUPPORT_DB_URL`). Service secret for internal
calls: `YC_SUPPORT_SERVICE_SECRET`. See `config/config.default.yaml` for
every setting.
