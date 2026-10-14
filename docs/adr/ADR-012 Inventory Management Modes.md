# ADR-012: Inventory Management Modes
**Status**: Accepted 
**Date**: 2025-01-15 
**Context**: Not all products need inventory tracking. Digital products (ebooks, downloads, licenses), services, and dropshipped items may be effectively infinite in stock.
## Decision
 - Inventory tracking is configurable at two levels: tenant and SKU.
### Levels
**Tenant-level**: inventory.enabled
- true (default): the tenant uses yc-inventory. SKUs may individually opt out via SKU-level config.
- false: the tenant does not use yc-inventory. All SKUs are treated as infinite stock. The RESERVING step is skipped entirely in the checkout saga.
**SKU-level**: inventory.tracked
Only meaningful when inventory.enabled = true at the tenant level.
 - true (default): stock levels tracked; reservations required; out-of-stock blocks checkout.
 - false: this SKU is treated as infinite stock (e.g., digital products in a mixed catalog).
### Checkout Saga Impact
 - At the RESERVING step:
 - `yc-checkout` partitions the cart's SKUs into tracked and untracked.
 - If all items are untracked (or tenant has inventory.enabled = false), the RESERVING step is skipped — no event to `yc-inventory`, state transitions directly to `AUTHORIZING`.
 - If any item is tracked, `yc-inventory` is called with only the tracked items.
 - On reservation failure for any tracked item, the entire checkout fails (confirms the "no partial checkout" decision).
### Partial Availability
- **Confirmed**: failing the whole checkout on any tracked item being out of stock. The user is told which specific items are unavailable so they can adjust the cart and retry.
No Backorders in v1
 - **Confirmed**: out-of-stock means unavailable. No backorder support. `yc-inventory` does not publish `backorder_available.v1` or similar events in v1.
## Events Affected
 - `inventory.reservation_requested.v1` carries only tracked SKUs.
 - If no tracked SKUs, the event is not published at all.
 - `product.sku.created.v1` and `product.sku.updated.v1` carry the `inventory.tracked` flag so `yc-inventory` knows whether to care about this SKU.
## Future Evolution
 - **Backorders**: new state in `yc-inventory` and new saga branch.
 - Low-stock warnings per SKU (already in taxonomy as `inventory.stock.low_threshold_reached.v1`).
 - **Multi-warehouse**: significant `yc-inventory` redesign, out of v1 scope.
