# ADR-015: Address Storage as JSON

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Address formats vary wildly by country. A rigid schema (street1/street2/city/state/zip) is US-centric and fails for many markets. The platform must support arbitrary tenant markets.

## Decision

Addresses are stored as **JSON documents** in all services (Orders, Checkout, Shipping). The structure is dictated by the **tenant's frontend**, not by backend schema.

## Rules

1. **Backend storage**: address is a `JSONB` column, no schema enforcement beyond "must be a JSON object"
2. **Backend validation**: format only — valid JSON, non-empty object, present when required
3. **No field-level validation** by backend services — the frontend owns address format
4. **The tenant frontend defines**:
   - What fields to collect (and in what order)
   - Field-level validation (postal code format, state picker, etc.)
   - How to render the address for display
   - Country-specific logic
5. **Rendering of addresses** (on invoices, shipping labels, order confirmations) is done by **templates owned by the tenant** in YC-Message and YC-Orders, not by hardcoded formatters
6. **Shipping integration**: when YC-Shipping integrates with carriers that require structured fields (e.g., international couriers), the carrier driver maps from the JSON to the carrier's expected format. The tenant frontend must collect the fields that driver requires.

## Address Metadata Convention

While the structure is free-form, we recommend (not enforce) a minimal convention for interoperability:

```json
{
  "_format": "ZA-residential",     // optional, tenant-defined identifier
  "country_code": "ZA",            // RECOMMENDED, ISO 3166-1 alpha-2
  "lines": ["Line 1", "Line 2"],   // RECOMMENDED, display-ordered
  "fields": {                       // free-form, tenant-defined
    "street": "...",
    "suburb": "...",
    "city": "...",
    "province": "...",
    "postal_code": "..."
  }
}
```

- `country_code` is **recommended** because YC-Shipping, YC-Pricing (tax), and some fraud checks will want it
- `lines` is **recommended** because YC-Message can render it without understanding the structure

Services may **soft-depend** on these recommended fields but must degrade gracefully when absent.

## Consequences

**Positive**
- No market is excluded by schema limitations
- Backend doesn't need to know about postal code formats, state codes, prefecture names, etc.
- Tenants fully control their checkout UX

**Negative**
- Shipping carrier drivers need to map from arbitrary JSON — requires tenant to collect the right fields
- Backend searches on address (e.g., "all orders to Cape Town") require either JSONB queries or a separate derived index
- Tax calculation that depends on region needs `country_code` (and often more) — the Pricing tax evaluator will **require** certain fields and reject the pricing if absent

## Required Fields by Function

Different functions have different minimum requirements. These are enforced by the **consuming service**, not by storage:

| Function | Minimum Required Fields |
|---|---|
| Order creation | Non-empty JSON object (if required by order type) |
| Tax calculation | `country_code` (always); additional fields depend on tax rules per country |
| Shipping quote | Fields required by the selected carrier driver |
| Invoice rendering | `lines` recommended; falls back to iterating `fields` |

The frontend is responsible for collecting the superset of fields needed for the tenant's enabled functions.

## Amends

### 2026-10-28 — Structured billing for invoices (issue #29)

**Delivery and shipping addresses remain free-form JSON** under the rules above.

**Customer and seller billing for tax invoices is an exception:** the platform validates a typed `BillingAddress` / `InvoiceAddress` schema (company name and VAT number optional; street and number, town/city, and postal code required when a billing object is present; suburb optional; `country_code` defaults to `ZA`). That schema is stored on the customer profile (yc-identity), optionally on the tenant as the seller `invoice_address`, and snapshotted onto the order at place time so later edits do not rewrite invoices.

This does not change ADR-015 for delivery, carrier mapping, or tax jurisdiction resolution from `delivery_address.country_code`.

### 2026-10-28 — Customer delivery address book (issue #32)

**Delivery documents remain free-form JSON** under the rules above.

yc-identity may store a **labelled address book** of those JSON documents per customer (soft-CRUD, max five active, edit = soft-retire + insert). Checkout and orders continue to quote and snapshot the JSON; orders also store optional `delivery_address_id` so the book can refuse update/retire while `Status.Open()` orders reference that id. Guests have no book. Billing remains the structured exception from the prior amendment.
