# ADR-024: Product Reviews

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Phase 7 adds yc-review. `yc-review.md` describes reviews, moderation, likes, verified purchases and Google reviews, but leaves open who may review, what "verified purchase" means when yc-orders cannot answer, whether an edited review is re-moderated, and how ratings and likes reach yc-product's search without yc-product counting events itself (and miscounting on redelivery or out-of-order delivery). It also leaves open how review prompts find out what a delivered order contained. The answers affect yc-orders, yc-product and yc-message, so they are recorded together.

## Decision

**A signed-in customer may review a product once, rated 1 to 5. The shop decides whether a purchase is required (by default it is), checked synchronously with yc-orders at submission, and whether reviews are moderated (by default manually). yc-review owns each product's review stats and publishes them, with a version, on every event that changes them; yc-product keeps the highest version it has seen.**

## Rules

1. **Who reviews.** Only customers write reviews and likes; anyone in the shop reads approved reviews and stats. One review per customer per product (a second is 409); ratings are whole numbers 1 to 5 with an optional title and body. The public list never shows the author's user ID.

2. **Verified purchase.** When the shop requires a purchase, yc-review asks yc-orders `GET /internal/purchases?user_id&product_id` (ADR-021) at submission: the customer's latest order with a line of the product that was not cancelled; guest orders do not count. A confirmed purchase marks the review `verified_purchase`. No purchase is 409 and nothing is stored. If yc-orders cannot answer, the review is stored `pending`, flagged `purchase_pending`, and checked again every 15 minutes for 24 hours: confirmed, it is verified (and approved in auto-approving shops); not bought, it is rejected as "unverified purchase"; still unanswered at the end, it is rejected as "purchase could not be verified". A review awaiting its purchase cannot be approved, and one rejected for its purchase cannot be edited; the author may delete it and submit again. When a purchase is not required, the check still runs to mark verified purchases, but neither a missing purchase nor an unreachable yc-orders holds the review back.

3. **Moderation.** Shops choose `manual` (default: every new or edited review is `pending` until a shop admin approves or rejects it) or `auto` (approved at once unless a purchase check is pending). Rejecting needs a reason, kept for the author, and may take down an approved review. A shop may turn off editing; when on, an edit goes back to moderation like a new review. The author may delete their review. The shop has one response per review; a new one replaces it.

4. **Stats are yc-review's.** Each product's approved-review count, rating sum and like count live in yc-review with a `version` that increases on every change, under a row lock. `review.submitted.v1`, `.edited.v1`, `.approved.v1`, `.rejected.v1`, `.deleted.v1`, `review.like.added.v1` and `.like.removed.v1` carry the product's stats after the change. yc-product stores them per product only when the version is higher than the one it has, so redelivered and out-of-order events cannot corrupt them, and it never counts. The average is `rating_sum / rating_count`; `GET /products?sort=rating` sorts by it (unrated last) and every product summary shows the count, average and likes.

5. **Likes.** A customer likes a product at most once (`PUT /likes/{product_id}`; repeating it changes nothing and publishes nothing); unliking is `DELETE`. A like is not a favourite: yc-product's favourites are the products a customer has ordered (`order.placed.v1`), and `order.placed.v1` also replaces `cart.item.added.v1` as the popularity signal (one point per order containing the SKU).

6. **Review prompts.** On `order.delivered.v1`, yc-review schedules a prompt 72 hours after delivery (configurable). When it is due, yc-review reads the order from yc-orders (`GET /internal/orders/{id}`), because the event carries no buyer type or product names; it prompts customers only, for the order's products they have not reviewed yet, by publishing `review.prompt_requested.v1` for yc-message (ADR-023). A missing order ends the prompt; an unreachable yc-orders delays it.

7. **Deferred.** Google reviews, review images, helpfulness votes and reporting abusive reviews are not part of Phase 7.

## Consequences

**Positive**
- "Verified purchase" means the same thing in every shop, and yc-orders stays the only source of what was bought.
- A short yc-orders outage does not lose reviews; they wait and are checked again.
- yc-product's ratings stay correct under at-least-once delivery without its own counters, and adding a stat later only needs a new field in the stats.
- Customers get one prompt per delivered order, without products they already reviewed.

**Negative**
- Submitting a review costs a call to yc-orders.
- A review whose purchase cannot be checked stays hidden for up to a day, then is rejected; the customer must delete it and submit it again.
- Every change to a product's reviews or likes takes a row lock on its stats, which serialises busy products.
- Prompts read the order when due, so a cancellation in the meantime ends the prompt rather than prompting for a cancelled order, but name changes since the order are not reflected.

## Alternatives Considered

- **Anyone may review, purchase shown as a badge only:** rejected as the default. Unverified reviews are easy to abuse; shops that want them turn off `require_purchase`.
- **Check purchases from events (yc-review keeps its own copy of orders):** rejected. It duplicates order history in another service, with its personal data and retention (ADR-005).
- **yc-product counts reviews and likes from events:** rejected. Redelivery and ordering would need per-event bookkeeping in yc-product; versioned totals make it a simple upsert.
- **Prompt from the delivery event alone:** rejected. The event has neither the buyer type nor the product names, and adding them would widen a public event for one consumer.
