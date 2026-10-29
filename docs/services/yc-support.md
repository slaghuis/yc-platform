# yc-support — Service Requirement

## Overview

`yc-support` owns customer support contact threads (issue #33 / Wisefood JRN-007). A customer or visitor opens a request, receives a unique acknowledgement reference, and exchanges messages with the shop until a shop admin closes the thread or it has been idle for 72 hours. Notifications go through yc-message; the shop is always emailed at the tenant `contact_email`.

Feature spec: `docs/features/Customer support contact threads.md`. Service README: `services/yc-support/README.md`.

## Location in Monorepo

```
/services/yc-support/
```

## Responsibilities

1. Accept support requests from customers and visitors (topic, contact details, first message)
2. Maintain a durable thread of messages with statuses `new` / `open_customer` / `open_shop` / `closed`
3. Let authenticated customers list, read and reply on their own threads (including claimed visitor threads)
4. Let shop admins list/filter, read, respond and close threads
5. Auto-close idle open threads after `support.idle_ttl` (default 72h from `last_activity_at`)
6. Claim unclaimed threads for a customer email after registration (`POST /internal/support/requests/claim`)
7. Notify requesters and the shop via `POST /internal/send` on yc-message
8. Publish support lifecycle events; participate in the GDPR deletion cascade

## Multi-Tenancy

- All request and message tables are tenant-scoped with RLS.
- Shop admins act on their shop only. Customers see only threads they own (`user_id`).
- Visitors may create once; they have no list/get until claim.

## Status machine

| From | Action | To |
|---|---|---|
| — | create | `new` |
| `new` / `open_shop` / `open_customer` | shop message | `open_customer` |
| `open_customer` | customer message | `open_shop` |
| `new` / `open_*` | shop close or idle sweeper | `closed` |

`closed` is final: further messages or a second close are `409`. There is no customer Close in MVP.

## API

| Method | Path | Auth |
|---|---|---|
| POST | `/support/requests` | visitor or customer |
| GET | `/support/requests` | customer |
| GET | `/support/requests/{id}` | customer (owner) |
| POST | `/support/requests/{id}/messages` | customer (owner) |
| GET | `/admin/support/requests` | shop_admin (filters: status, topic, email, reference) |
| GET | `/admin/support/requests/{id}` | shop_admin |
| POST | `/admin/support/requests/{id}/messages` | shop_admin |
| POST | `/admin/support/requests/{id}/close` | shop_admin |
| POST | `/internal/support/requests/claim` | service (`{user_id, email}`) |

Contract: `services/yc-support/api/openapi.yaml`.

## Notifications

| yc-message type | Recipient | When |
|---|---|---|
| `support_ack` | requester | created |
| `support_new_request` | shop `contact_email` | created |
| `support_shop_reply` | requester | shop message |
| `support_customer_reply` | shop | customer/visitor message |
| `support_closed` | requester and shop | staff or idle close |

Customers are addressed by `user_id` (channel resolution, ADR-023). Visitors by `contact_email` only. Customer emails may include `/account/support/{id}` (ADR-027); visitor emails do not. MVP uses sync `/internal/send` until templates subscribe to events.

## Events Published

On the `YC_SUPPORT` stream (1-year retention):

| Event | When |
|---|---|
| `support.request.created.v1` | Request opened |
| `support.message.posted.v1` | Subsequent message (or system idle-close note) |
| `support.request.closed.v1` | Staff or idle close |

Payloads carry IDs, reference and statuses only. Schemas: `schemas/yc-event-schemas/support/`.

## Events Consumed

| Event | Action |
|---|---|
| `identity.user.deleted.v1` | Pseudonymise `user_id`, redact contact fields and message bodies; publish `platform.gdpr.cascade_completed.v1` |

## Dependencies

- **yc-message** — transactional notifications (`/internal/send`)
- **yc-tenant** — shop `contact_email`
- **yc-identity** — calls claim after register / OAuth register
- **yc-gateway** — public and internal routing; service tokens (ADR-021)

## Configuration

| Key | Purpose |
|---|---|
| `database.url` | `YC_SUPPORT_DB_URL` |
| `internal_calls.client_secret` | `YC_SUPPORT_SERVICE_SECRET` (≥32 chars) |
| `support.idle_ttl` / `sweep_interval` / `sweep_batch` | Idle auto-close sweeper |
| `support.storefront_base` | Optional prefix for `/account/support/{id}` links |
| `tenant.contact_cache_ttl` | Cache for shop contact email |
| `gateway.*` | Self-registration (ADR-020 rule 7) |

## Not In Scope (MVP)

- Attachments / malware scanning
- Staff roles beyond `shop_admin`
- Inbound email → thread
- Customer-initiated Close or reopen
- Per-topic routing addresses
