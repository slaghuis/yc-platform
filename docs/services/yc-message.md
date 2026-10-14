# yc-message — Service Requirement

## Overview

`yc-message` is the messaging gateway. It accepts message requests from other services, routes them to the right channel for the recipient (based on yc-preference), handles templating and localization, delivers via channel-specific providers (SMTP, SMS gateway, WhatsApp Business API, Telegram), tracks delivery, and implements an outbox pattern to guarantee no message is lost.

## Location in Monorepo

```
/services/yc-message/
```

## Responsibilities

1. Accept message send requests from other services
2. Resolve the recipient's preferred channel (via yc-preference)
3. Fall back to email if preferred channel is unverified or unavailable
4. Load templates per message type, with localization
5. Render templates with context data
6. Deliver via channel-specific provider drivers
7. Track delivery, failures, bounces, opens (if provider supports)
8. Store all sent messages for audit
9. Verify new channels (send verification code, call yc-preference on confirmation)
10. Emit lifecycle events
11. Respect customer messaging consent

## Multi-Tenancy

- Templates are tenant-scoped (with platform-level defaults as fallback)
- Provider credentials are tenant-scoped
- Message history is tenant-scoped

## Channels Supported in v1

| Channel | Driver | Notes |
|---|---|---|
| `email` | SMTP (SendGrid, SES, Mailgun as drivers) | Mandatory for v1 |
| `sms` | Twilio, Clickatell, or similar SA SMS provider | v1 target |
| `whatsapp` | WhatsApp Business API (via Twilio or 360dialog) | v1 nice-to-have; stub if deferred |
| `telegram` | Telegram Bot API | Future |

Each channel is implemented as a driver behind:

```go
type ChannelDriver interface {
    Name() string
    Send(ctx, SendRequest) (SendResponse, error)
    ParseWebhook(ctx, WebhookRequest) (DeliveryEvent, error)
}
```

## Channel Resolution

```
Resolve channel for user:
  if message has override_channel: use it
  else:
    channel = yc-preference.primary_channel for user
    if channel unverified OR driver disabled: fallback to email
    if email also unverified AND user is customer: queue, retry after 1h
    if guest (no preference record): use whatever contact info is present
      (email from guest_contact, SMS from guest_contact.phone)
```

## Templates

- Templates are stored per `(tenant_id, message_type, channel, locale)`
- A platform-wide default template exists for every message type as fallback
- Rendering uses Go `text/template` (or `html/template` for HTML emails)
- Template context is message-type-specific; schemas are documented per message type

### Example Message Types (v1)

| Message Type | Trigger |
|---|---|
| `welcome` | `identity.user.registered.v1` |
| `password_reset` | `identity.user.password_reset_requested.v1` |
| `channel_verification` | New channel added |
| `order_confirmation` | `order.placed.v1` |
| `order_shipped` | `order.shipped.v1` |
| `order_delivered` | `order.delivered.v1` |
| `order_cancelled` | `order.cancelled.v1` |
| `refund_issued` | `order.refunded.v1` |
| `cart_abandoned` | `cart.abandoned.v1` |
| `review_prompt` | `review.prompt_requested.v1` |
| `back_in_stock` | `inventory.back_in_stock.notified.v1` |
| `low_stock_admin` | `inventory.stock.low_threshold_reached.v1` |
| `chargeback_admin` | `payment.chargeback_received.v1` |
| `delivery_exception` | `shipping.delivery_exception.v1` |

## Delivery Flow

```
yc-message consumes event (e.g., order.placed.v1)
  1. Determine message type
  2. Build context (fetch extra data if needed via internal APIs)
  3. Resolve recipient and channel
  4. Load template
  5. Render
  6. Persist as message_queued
  7. Call driver.Send
  8. On success: message_sent; await webhook for delivery confirmation
  9. On failure: retry via outbox with backoff
  10. After max retries: message_failed; alert if critical type
```

## Delivery Status Tracking

Webhooks from providers update status:

```
queued → sent → delivered → opened (email only, best-effort)
     │     │
     │     └─▶ bounced → yc-preference.channel.verified = false
     │
     └─▶ failed (retried via outbox)
```

## Marketing vs Transactional

- **Transactional** messages: order confirmations, password resets, verifications — sent regardless of marketing consent
- **Marketing** messages: cart abandonment, promotional campaigns — require consent
- v1 scope: transactional only. Marketing campaigns future.
- The `cart_abandoned` message is positioned as transactional in v1; tenants should consult local regulations.

## API

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/send` | Direct send request (bypasses event trigger) | service |
| POST | `/internal/channels/verify-start` | Begin channel verification | service |
| POST | `/internal/channels/verify-confirm` | Complete verification with code | service |
| GET | `/internal/messages/{id}` | Message detail | service |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/messages` | List messages (filterable) | shop_admin |
| GET | `/admin/messages/{id}` | Message detail | shop_admin |
| GET | `/admin/templates` | List tenant templates | shop_admin |
| PUT | `/admin/templates/{type}/{channel}/{locale}` | Set/update a template | shop_admin |
| GET | `/admin/providers` | List provider configs | shop_admin |
| PUT | `/admin/providers/{channel}` | Configure provider | shop_admin |
| POST | `/admin/providers/{channel}/test` | Send test message | shop_admin |

### Webhook Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhooks/{driver}` | Provider delivery webhook |

## Database Schema

Database: **`yc_message`**

```sql
CREATE TABLE provider_configs (
    tenant_id       TEXT NOT NULL,
    channel         TEXT NOT NULL,
    driver          TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    credentials_enc BYTEA,
    config          JSONB NOT NULL DEFAULT '{}',
    from_address    TEXT,                   -- email from, SMS sender ID, etc.
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, channel)
);

CREATE TABLE templates (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,          -- '_platform' for defaults
    message_type    TEXT NOT NULL,
    channel         TEXT NOT NULL,
    locale          TEXT NOT NULL,
    subject         TEXT,                    -- email only
    body            TEXT NOT NULL,
    body_format     TEXT NOT NULL,           -- 'text','html','markdown'
    version         INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, message_type, channel, locale)
);

CREATE TABLE messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    message_type    TEXT NOT NULL,
    recipient_user_id TEXT,                   -- or NULL for guests
    recipient_address TEXT NOT NULL,          -- resolved address (email, phone)
    channel         TEXT NOT NULL,
    locale          TEXT NOT NULL,
    subject         TEXT,
    body_rendered   TEXT NOT NULL,
    status          TEXT NOT NULL,            -- 'queued','sent','delivered','bounced','opened','failed'
    driver          TEXT NOT NULL,
    provider_message_id TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    context         JSONB,                     -- template context used
    source_event_id UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ
);
CREATE INDEX idx_messages_tenant_recipient ON messages (tenant_id, recipient_user_id, created_at DESC);

CREATE TABLE verification_codes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    channel         TEXT NOT NULL,
    code_hash       TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE messages ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON messages
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
```

## Published Events

| Event | When |
|---|---|
| `message.queued.v1` | Message persisted, ready to send |
| `message.sent.v1` | Handed to provider |
| `message.delivered.v1` | Provider confirmed delivery |
| `message.opened.v1` | Email opened (best-effort) |
| `message.bounced.v1` | Hard bounce — triggers channel unverification |
| `message.failed.v1` | Exhausted retries |
| `message.verification_sent.v1` | Verification code dispatched |
| `message.verification_confirmed.v1` | User entered correct code |

## Subscribed Events

Driven by nearly every service. Each subscription maps event → message type.

| Event | Message Type |
|---|---|
| `identity.user.registered.v1` | `welcome` |
| `identity.user.password_reset_requested.v1` | `password_reset` |
| `order.placed.v1` | `order_confirmation` |
| `order.shipped.v1` | `order_shipped` |
| `order.delivered.v1` | `order_delivered` |
| `order.cancelled.v1` | `order_cancelled` |
| `order.refunded.v1` | `refund_issued` |
| `cart.abandoned.v1` | `cart_abandoned` |
| `review.prompt_requested.v1` | `review_prompt` |
| `inventory.back_in_stock.notified.v1` | `back_in_stock` |
| `inventory.stock.low_threshold_reached.v1` | `low_stock_admin` (routed to shop admin) |
| `payment.chargeback_received.v1` | `chargeback_admin` (routed to shop admin) |
| `shipping.delivery_exception.v1` | `delivery_exception` |
| `preference.channel.added.v1` | Trigger channel verification |
| `identity.user.deleted.v1` | Anonymize `recipient_address` on historical messages beyond operational window; publish `platform.gdpr.cascade_completed.v1` |

## Configuration

```yaml
yc-message:
  listen: :8080
  database:
    url: postgres://yc_message:...@...
  encryption:
    credentials_cipher_key_env: YC_MESSAGE_CIPHER_KEY
  defaults:
    retry_max_attempts: 5
    verification_code_ttl_minutes: 15
    verification_code_length: 6
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-preference** (recipient channel resolution)
- **yc-identity** (user info for templating)
- **yc-orders** (order details for order emails)
- External: SMTP, SMS, WhatsApp providers

## Called By

- Virtually all services (via events)
- Shop admins (template management, test sends)

## GDPR Behavior

- Message history is PII (contains address, rendered content with personal data)
- On `identity.user.deleted.v1`: anonymize `recipient_user_id`, hash `recipient_address`, retain delivery audit for 90 days then purge content
- Verification codes deleted immediately on expiry or use

## Open Questions / Future Scope

- Marketing campaign engine (segmentation, scheduling)
- A/B testing of templates
- Rich email editor for shop admins (WYSIWYG)
- Push notifications (mobile apps)
- In-app notifications
- Scheduled messages / drip campaigns
- Rate limiting per recipient (prevent flooding)
- Unsubscribe link management
- Shared template library across tenants
