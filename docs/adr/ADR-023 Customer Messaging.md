# ADR-023: Customer Messaging

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Phase 7 sends the platform's first customer emails: welcome, order confirmation, shipping, delivery, cancellation, refund, abandoned cart, review prompts, password resets and channel verification codes. `yc-message.md` lists many providers (SMTP, SendGrid, Twilio, WhatsApp, Telegram) with per-shop credentials, but not how a message finds its recipient, which template and language it uses, what happens when a provider is down, or how secrets such as reset tokens reach yc-message without crossing the event bus (ADR-001: events are readable by every subscriber of the domain). Channel verification spans yc-preference, which owns the addresses, and yc-message, which sends the code. These choices touch identity, preference, orders, cart and review, so they are recorded together.

## Decision

**yc-message turns events into queued messages, addressed from yc-preference (customers) or the order's contact (guests), rendered from shop templates that fall back to embedded platform defaults, and delivered by a dispatcher with retries. Secrets never travel as events: yc-identity calls `POST /internal/send` for password resets, and verification codes are generated, hashed and checked inside yc-message.**

## Rules

1. **Events become messages.** yc-message subscribes to `identity.user.registered.v1` (welcome), `order.placed.v1`, `order.shipped.v1`, `order.delivered.v1`, `order.cancelled.v1`, `order.refunded.v1`, `cart.abandoned.v1`, `review.prompt_requested.v1` and `preference.channel.added.v1` (verification code). Each event queues at most one message per type: the message's `reference` is `event:<event id>:<type>` and is unique per shop, so redelivery is harmless. Order messages read the order from yc-orders (`GET /internal/orders/{id}`) for its number, customer or guest contact. A recipient with no usable address is logged and skipped; an upstream error is retried by the bus.

2. **Recipients.** A customer is reached on their primary channel when it is verified and has a driver; otherwise by email, verified or not, because the account's email is how the shop reaches them; else SMS. The address comes from yc-preference (`GET /internal/preferences/{user_id}`), and so does the locale. A guest (ADR-010) is reached at the order's contact email, else its phone. The welcome email always goes to the registered email.

3. **Templates.** A template is a subject (email only) and body per message type, channel and locale, rendered with Go `text/template` (or `html/template` for HTML bodies); a missing key fails the render rather than sending `<no value>`. The shop's own template for the recipient's locale wins, then its language (`en-ZA` → `en`), then the shop's `en`, then the **platform default**, which is embedded in the binary in `en`, not seeded into the database: a release can improve the defaults for every shop that has not overridden them. Shop admins list, override, delete and test-send templates (`/admin/templates/...`); an override is rejected unless it renders against the type's sample context. The rendered subject and body are stored on the message.

4. **Delivery.** A dispatcher picks due messages (status `queued`, `next_attempt_at` passed), locks each row (`FOR UPDATE SKIP LOCKED`) while sending, so two replicas never send one message twice, and wakes as soon as a message is queued. Success marks it `sent` and publishes `message.sent.v1`. A transient error retries with exponential backoff (30 s doubling, at most an hour); a permanent refusal, a missing driver or the fifth attempt marks it `failed` and publishes `message.failed.v1`. Email goes through the platform SMTP relay (MailHog in development). SMS uses the `fake` driver in development, which mails each SMS to `<number>@sms.test`; its unsigned `POST /webhooks/fake-sms` reports `delivered` or `bounced` and exists only when enabled. A bounce publishes `message.bounced.v1`. Per-shop provider credentials and real SMS, WhatsApp and Telegram providers are deferred.

5. **Channel verification.** When a customer adds an email or SMS channel, yc-message sends a 6-digit code, stored only as a SHA-256 hash salted with the verification's ID. It expires after 15 minutes and allows 5 tries; a new code may be requested after a minute (`POST /preferences/me/channels/{channel}/resend`). The customer submits the code to yc-preference (`POST /preferences/me/channels/{channel}/verify`), which checks it with yc-message (`POST /internal/channels/verify-confirm`), marks the channel verified and publishes `preference.channel.verified.v1`; yc-message publishes `message.verification_confirmed.v1`, which yc-preference also applies, idempotently. A wrong code is 400; an expired or exhausted one is 409. When `message.bounced.v1` names a customer's address, yc-preference unverifies it, and a bounced primary channel other than email falls back to email.

6. **Secrets stay off the bus.** `identity.user.password_reset_requested.v1` carries no token. After committing the reset request, yc-identity calls yc-message `POST /internal/send` (ADR-021) with the reset link, with `Idempotency-Key` and `reference` `password-reset:<request id>`; a failed call is logged, never shown, so the endpoint still does not reveal whether an email is registered. Once a password reset or verification message is sent or failed, its body and context are replaced with `[redacted]`.

7. **History.** Shop admins list and read their shop's messages (`GET /admin/messages`, filtered by status, type or customer). Admin alerts (low stock, failed payments) and marketing messages are not part of this decision.

## Consequences

**Positive**
- Services only publish facts; adding a message type touches yc-message and its templates, not the publisher.
- Shops change wording and languages without a release, and every shop gets improved defaults with one.
- A provider outage delays messages instead of losing them, and the history shows what was sent, failed or bounced.
- Reset tokens and verification codes never reach NATS, logs or the message history.

**Negative**
- Order messages make a call to yc-orders and customer messages one to yc-preference; when either is down, the event is retried later and the email is late.
- Platform-wide SMTP means one sender domain for every shop until per-shop credentials arrive.
- A customer who never verifies a channel still gets email at their account address; SMS needs a verified channel or a guest phone number.

## Alternatives Considered

- **Publishers send their own email:** rejected. Every service would need templates, providers and retries, and there would be no single history.
- **Seed default templates into each shop's database:** rejected. Improving a default would need a migration per shop and could overwrite a shop's edits.
- **Put the reset token in the event:** rejected. Every subscriber of the identity domain, and the stream's retention, would see a credential.
- **Verify codes in yc-preference:** rejected. yc-message generates and sends the code; keeping the hash with the sender means the code never leaves the service that created it.
