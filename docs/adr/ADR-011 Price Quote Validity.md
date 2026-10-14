# ADR-011: Price Quote Validity
**Status**: Accepted 
**Date**: 2025-01-15 
**Context**: Between the PRICING step (quote shown to user) and the CONFIRM step (user accepts), prices or promotions may change. We must decide whether to re-price or honor the quote.
## Decision
 - Honor the original quote for the full checkout session.
## Rules
 - The default session validity is 30 minutes, configurable per tenant.
 - Prices, taxes, shipping costs, and applied promotions snapshotted at PRICING are immutable for the session.
 - If the user confirms within the window, those prices are the ones charged.
 - If the user does not confirm within the window:
 - The session enters EXPIRED state.
 - A fresh checkout must be started with current prices.
 - The 30-minute default is enforced by yc-checkout; tenants may override via a tenant configuration (checkout.session_ttl_minutes), with minimum 10 min and maximum 60 min.
 - If a promotion ends during a live session, affected sessions are not retroactively repriced — the quote wins.
 - If the base price changes (shop admin updates a price) during a live session, affected sessions are not retroactively repriced.
## Rationale
 - Better UX — no "price just changed" surprises at the moment of commitment.
 - 30 minutes is bounded financial risk for the tenant.
 - Inventory reservation TTL (15 min) is shorter than session TTL (30 min) — if reservation expires first, that triggers the natural failure path.
## Consequences
### Positive
 - Clean UX.
 - Simple saga logic (no re-pricing branch).
 - Pricing snapshot in checkout session is authoritative.
### Negative
 - Potential narrow loss on a tenant where prices drop during the window (negligible).
 - Potential narrow loss on a tenant where a flash promotion starts (user doesn't benefit; refresh gives them the new price).
