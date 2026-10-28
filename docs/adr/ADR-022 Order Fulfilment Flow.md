# ADR-022: Order Fulfilment Flow

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Phase 6 connects checkout, orders and shipping. The service specifications disagree on how: `yc-shipping.md` creates shipments from `order.placed.v1` and fetches the order, `yc-orders.md` has the shop admin mark orders packaged and lists `POST /internal/shipments`, and `yc-checkout.md` quotes shipping without saying when the customer gives the address. Checkout must also know the shipping amount before confirm, because the payment covers it (ADR-019). Returns and partial refunds are listed in yc-orders but not specified. Each choice affects several services, so they are recorded together.

## Decision

**Shipping is quoted synchronously at checkout's shipping step and booked synchronously when the shop admin packages the order; from then on, yc-shipping owns the shipment's status and yc-orders follows it through events.** Returns are decided in yc-orders and refunded through yc-payment straight away.

## Rules

1. **Quote at the shipping step.** `POST /checkout/{id}/shipping` takes the delivery address (free-form, ADR-015; only checked to be a non-empty object of reasonable size) and calls yc-shipping `POST /internal/quote` with the physical and digital lines, each SKU's `weight_grams` and the items total. yc-shipping returns the options, cheapest first; checkout keeps them with the address and selects the named option or the cheapest. A later call with only `shipping_option_id` chooses among the same options; a new address replaces them. No options means 409 "The shop does not ship to this address", and the session is unchanged. Shipping is untaxed.

2. **Confirm uses that address.** Physical items cannot be confirmed before the shipping step. Confirm may repeat the address; a different one (compared after trimming strings, ignoring key order) is 409, so the customer never pays for shipping quoted to somewhere else. An all-digital checkout has no shipping step and needs no address (ADR-010).

3. **Zones and rates.** A zone has rules of a `country_code` and optional exact `fields` values (case-insensitive). Zones are tried by priority (lowest first, then name); the first that matches wins, and only its rates are offered. A rate is one method per zone, priced:
   - `flat`;
   - `weight_tiered`: the first tier whose `max_weight_g` covers the parcel, where the parcel weight is the sum of the physical lines' weights. The rate is not offered when a physical SKU has no weight, or the parcel is heavier than the last tier;
   - `price_tiered`: the last tier whose `min_subtotal_minor` the items total (gross) reaches.

   Amounts are in the store currency (ADR-014). SKU dimensions (millimetres) are stored by yc-product for future carrier drivers; no rate uses them yet.

4. **Booking by call.** `POST /admin/orders/{id}/package` (yc-orders) calls yc-shipping `POST /internal/shipments` with the order number, chosen method and rate, and delivery address, with `Idempotency-Key: order:<id>:package`. yc-shipping keeps one shipment per order (a repeat returns it), asks the rate's carrier driver for the tracking number and label, and publishes `shipping.label_created.v1`. yc-orders then locks the order, re-checks that it is still `received`, stores the shipment ID and tracking number, moves it to `packaged` and publishes `order.packaged.v1`. Digital orders are never packaged.

5. **Progress by events.** The shop admin updates a `manual` shipment in yc-shipping (`POST /admin/shipments/{id}/manual-update`: `in_transit`, `delivered`, `exception`); carrier drivers will report the same statuses. yc-shipping publishes `shipping.in_transit.v1`, `shipping.delivered.v1` and `shipping.delivery_exception.v1`, and yc-orders moves the order to `shipped`, `delivered` or `delivery_exception`, publishing `order.shipped.v1` and `order.delivered.v1`. Each event type has its own durable consumer, so they can arrive out of order: `delivered` is accepted from `packaged`, `delivery_exception` recovers to `shipped` or `delivered`, and an event that would move the order backwards is dropped (its carrier details are still recorded). Events for another shipment of the order are ignored.

6. **Cancellation.** An order can be cancelled while `received` or `packaged`. yc-shipping cancels a shipment that has not left the shop when it sees `order.cancelled.v1`, and publishes `shipping.cancelled.v1`. That also covers a cancel that races a booking: the shipment booked for a cancelled order is cancelled by the event.

7. **Returns.** The order's owner may ask to return physical lines, wholly or partly, within 30 days of delivery; quantities are limited to what earlier accepted or open requests leave, and an order has at most one open request. The shop admin accepts or rejects it (a reason is required to reject). Accepting refunds at once through yc-payment `POST /internal/refund` with `Idempotency-Key: order:<order>:return:<return>`: each line's share of its line total, rounded so the shares of a line add up to it, plus the order's shipping if the admin says so (at most once per order), capped at what is left to refund. There is no return shipment and no restock in v1. When every physical line has come back, the order becomes `returned`.

8. **Refunds.** Every refund is recorded in yc-orders with the key it was sent to yc-payment with, unique per order, so a retried accept or admin refund records it once; `refunded` is the sum, and `order.refunded.v1` is published. The admin may also refund any amount up to what is left (`POST /admin/orders/{id}/refund`, `Idempotency-Key` required).

9. **Public tracking.** `GET /shipping/tracking/{tracking_number}` shows a shipment's status and history to anyone with the number; it shows no address or order details.

## Consequences

**Positive**
- The customer sees the exact shipping amount before paying, priced by the service that owns zones and rates.
- The admin's packaging response carries the tracking number, and a failed booking leaves the order `received` to try again.
- yc-shipping never needs to read orders; yc-orders never needs to know carriers.
- Carrier integrations later change only yc-shipping: they report the same statuses through the same events.

**Negative**
- Packaging depends on yc-shipping being up; the admin retries if it is not.
- An address change after the shipping step needs another quote.
- Order status follows shipping events with a short delay (eventual consistency), so the admin sees `packaged` until yc-orders has applied `shipping.in_transit.v1`.
- Returns refund on acceptance, before any goods come back; shops that want to inspect first must wait to accept.

## Alternatives Considered

- **Create shipments from `order.placed.v1`** (the original yc-shipping specification): rejected. It books a label before the shop has packed anything, so cancelling a received order would always need a carrier cancellation, and the admin gets no tracking number when packaging.
- **Quote at confirm:** rejected. The payment amount would change at the moment of paying, and a failed quote would become a failed confirm.
- **yc-orders stores the shipment status directly from admin calls:** rejected. It would duplicate the shipment lifecycle and leave no place for carrier webhooks.
- **Return shipments and restocking in v1:** deferred. They need a return label from a carrier driver and an inventory receipt, neither of which exists yet.

## Amends

### 2026-10-28 — Delivery slots (issue #30)

Shops may attach a **delivery schedule** to a rate (`requires_delivery_slot` + `schedule_id`). After the shipping quote (rule 1), checkout lists generated time windows for that rate and the customer selects one (`PUT /checkout/{id}/slot`, which must still have remaining capacity). Confirm requires a bound slot when the selected rate demands it; a stale or full window is **409**.

Capacity is **booked atomically at ORDER_CREATE** (keyed by checkout session) and **released on abandon/compensate**. The bound window is snapshotted on the session and order (`delivery_slot`); later schedule edits do not rewrite history. Digital checkouts and rates without the flag are unchanged. Admin reschedule after place, slot surcharges, and `order.placed.v1` schema changes are out of this amendment.
