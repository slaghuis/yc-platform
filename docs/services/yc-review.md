# yc-review — Service Requirement

## Overview

`yc-review` manages customer-authored product reviews and likes, moderation of those reviews, and caches external Google reviews about the shop. It feeds yc-product's search projection with rating and like signals.

## Location in Monorepo

```
/services/yc-review/
```

## Responsibilities

1. Accept product reviews from customers (one review per customer per product, editable)
2. Attach optional images to reviews (via yc-image references)
3. Moderate reviews via a workflow (pending → approved | rejected)
4. Allow shop admins to moderate; system admins have override
5. Allow customers to like/unlike products
6. Compute aggregate rating stats per product
7. Fetch and cache Google reviews for the shop (per tenant configuration)
8. Emit review lifecycle events for yc-product's search projection

## Multi-Tenancy

- Reviews, likes, moderation, and Google review cache are tenant-scoped
- A review is **product-level**, not SKU-level (per the original spec)
- A user may have one review per `(tenant_id, product_id)`

## Moderation Workflow

```
submitted ─▶ pending ──▶ approved
                   │
                   └──▶ rejected
```

- Moderation can be:
  - **Manual** (default): shop admin reviews each submission
  - **Auto-approve** (tenant option): reviews go to `approved` immediately; shop admin can still reject post-hoc
  - **Auto-approve-with-flagging** (future): heuristic/profanity check runs; flagged reviews go to pending, others auto-approve
- Rejected reviews are retained with reason (audit) but hidden from public API
- Customers can edit their own review; editing moves it back to pending (configurable)
- Shop admins can respond to a review (one response per review)

## Rating Rules

- Rating scale: **1 to 5 integers**
- Each review has: rating (required), title (optional), body (optional), image references (optional)
- Reviews require a verified purchase by default (configurable per tenant: `require_purchase = true`)
- "Verified purchase" is determined via yc-orders — yc-review calls yc-orders to confirm; a failure to confirm within 24 hours of submission rejects the review automatically

## Likes

- Simple binary: user has or has not liked a product
- Unlike removes the record
- Like/unlike is instant, no moderation
- Like counts feed yc-product's search projection

## Google Reviews Cache

- Per-tenant configuration: enable, Google Place ID, API key (encrypted)
- A background job fetches reviews daily (configurable interval)
- Stores up to N reviews (configurable, default 20) with timestamp
- Exposed via public API alongside product reviews
- Google ToS compliance: display attribution, don't modify content

## API

### Public Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/products/{id}/reviews` | List approved reviews for a product | any |
| GET | `/products/{id}/reviews/stats` | Rating distribution and aggregates | any |
| POST | `/products/{id}/reviews` | Submit a review | customer |
| PUT | `/reviews/{id}` | Edit own review | customer (owner) |
| DELETE | `/reviews/{id}` | Delete own review | customer (owner) |
| POST | `/products/{id}/likes` | Like a product | customer |
| DELETE | `/products/{id}/likes` | Unlike a product | customer |
| GET | `/shop/google-reviews` | List cached Google reviews for the tenant | any |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/reviews` | List reviews (filterable by status) | shop_admin |
| POST | `/admin/reviews/{id}/approve` | Approve a review | shop_admin |
| POST | `/admin/reviews/{id}/reject` | Reject a review (with reason) | shop_admin |
| POST | `/admin/reviews/{id}/respond` | Add shop response | shop_admin |
| PUT | `/admin/moderation-config` | Set tenant moderation mode | shop_admin |
| PUT | `/admin/google-reviews-config` | Set Google reviews integration | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/internal/reviews/stats/{product_id}` | Aggregate stats | service |
| GET | `/internal/user/{user_id}/has_purchased/{product_id}` | Not used — called OUT to yc-orders instead | — |

## Database Schema

Database: **`yc_review`**

```sql
CREATE TABLE tenant_review_config (
    tenant_id            TEXT PRIMARY KEY,
    moderation_mode      TEXT NOT NULL DEFAULT 'manual',  -- 'manual','auto'
    require_purchase     BOOLEAN NOT NULL DEFAULT TRUE,
    allow_edit           BOOLEAN NOT NULL DEFAULT TRUE,
    google_place_id      TEXT,
    google_api_key_enc   TEXT,                             -- encrypted
    google_refresh_hours INT NOT NULL DEFAULT 24,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE reviews (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    product_id      UUID NOT NULL,
    user_id         TEXT NOT NULL,
    rating          SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    title           TEXT,
    body            TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',       -- 'pending','approved','rejected'
    rejection_reason TEXT,
    verified_purchase BOOLEAN NOT NULL DEFAULT FALSE,
    shop_response   TEXT,
    shop_response_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, product_id, user_id)
);
CREATE INDEX idx_reviews_product ON reviews (tenant_id, product_id, status);

CREATE TABLE review_images (
    review_id       UUID NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
    image_id        UUID NOT NULL,
    sort_order      INT NOT NULL DEFAULT 0,
    PRIMARY KEY (review_id, image_id)
);

CREATE TABLE product_likes (
    tenant_id       TEXT NOT NULL,
    product_id      UUID NOT NULL,
    user_id         TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, product_id, user_id)
);
CREATE INDEX idx_product_likes_product ON product_likes (tenant_id, product_id);

CREATE TABLE google_reviews_cache (
    tenant_id       TEXT NOT NULL,
    review_hash     TEXT NOT NULL,          -- hash of (author, time, text) for dedupe
    author_name     TEXT,
    rating          SMALLINT,
    text            TEXT,
    language        TEXT,
    review_time     TIMESTAMPTZ,
    cached_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, review_hash)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE reviews ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON reviews
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Published Events

| Event | When |
|---|---|
| `review.submitted.v1` | New review (goes to pending or auto-approved) |
| `review.approved.v1` | Review approved (triggers search projection update) |
| `review.rejected.v1` | Review rejected |
| `review.edited.v1` | Author edited (may re-enter pending) |
| `review.deleted.v1` | Author or admin deleted |
| `review.responded.v1` | Shop admin added response |
| `review.like.added.v1` | Product liked |
| `review.like.removed.v1` | Product unliked |
| `review.google_reviews.refreshed.v1` | Google reviews cache updated |
| `review.prompt_requested.v1` | Review prompt due for a delivered order (after the configured delay; consumed by yc-message) |

## Subscribed Events

| Event | Action |
|---|---|
| `order.delivered.v1` | Optional: prompt customer via yc-message to review (via a separate `review.prompt_requested.v1` emitted by yc-review after delay) |
| `identity.user.deleted.v1` | Anonymize reviews (set user_id to pseudonym, strip author display name); delete likes; publish `platform.gdpr.cascade_completed.v1` |
| `product.deleted.v1` | Delete reviews and likes for the product |
| `platform.tenant.provisioned.v1` | Create `tenant_review_config` with defaults |
| `platform.tenant.deleted.v1` | Delete all tenant reviews |

### Verified Purchase Check

When a review is submitted with `require_purchase = true`:
- yc-review calls yc-orders internal API: `GET /internal/orders/user/{user_id}/has_purchased_product/{product_id}`
- Response: `{ purchased: true/false, last_delivered_at: ... }`
- If `true`: `verified_purchase = true` on the review
- If `false`: review is rejected with reason "unverified purchase"
- If call fails: review stays in `pending`; retried for 24h; after that, auto-rejected

## Configuration

```yaml
yc-review:
  listen: :8080
  database:
    url: postgres://yc_review:...@...
  defaults:
    moderation_mode: manual
    require_purchase: true
    allow_edit: true
    google_refresh_hours: 24
  review_prompt_after_delivery_hours: 72
  encryption:
    google_key_cipher_key_env: YC_REVIEW_GOOGLE_CIPHER_KEY
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-orders** (verified purchase check — soft dependency; failures retried)
- **yc-image** (image references)
- External: Google Places API (optional, per tenant)

## Called By

- Clients (reviews & likes)
- Shop admins (moderation)
- `yc-product` (consumes events for search projection)
- `yc-message` (consumes `review.prompt_requested.v1` to send review requests)

## GDPR Behavior

- Reviews contain PII (`user_id`, author display name, free-text body)
- On `identity.user.deleted.v1`: anonymize reviews — set `user_id` to pseudonym, replace display name with "Deleted User", retain review content for integrity; delete likes
- Publish `platform.gdpr.cascade_completed.v1`
- Review images (via yc-image) are deleted via a `review.deleted.v1` or `review.rejected.v1` event

## Open Questions / Future Scope

- Review helpfulness votes ("Was this helpful?")
- Review photos moderation (image content check)
- Multi-language review translation
- Review spam detection (automated)
- Verified purchase enforcement by SKU (not just product)
- Review incentives (loyalty points for reviewing)
- Rich review formatting (markdown, pros/cons)
- Reply threads on reviews (customer ↔ shop admin dialogue)
