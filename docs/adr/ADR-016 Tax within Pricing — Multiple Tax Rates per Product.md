# ADR-016: Tax within Pricing — Multiple Tax Rates per Product

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Tax remains inside the Pricing pipeline rather than a dedicated service for v1. However, tax is not uniform — different products attract different tax rates (zero-rated goods, VAT-exempt services, luxury goods, excise duties).

## Decision

Tax is a **Pricing evaluator** in YC-Pricing. Each product/SKU carries a **tax classification** that determines which tax rate(s) apply.

## Model

### Tax Classes

Each tenant defines a set of **tax classes** per jurisdiction. Examples for South Africa:

- `za-standard` — 15% VAT
- `za-zero-rated` — 0% VAT (basic foodstuffs, exports, etc.)
- `za-exempt` — no VAT applies (financial services, residential rent)

Tax classes are configured in YC-Pricing per tenant.

### SKU Tax Classification

Each SKU carries a **tax class reference**. This is set when the product/SKU is created in YC-Product and **synchronized to YC-Pricing** via the `product.sku.created.v1` and `product.sku.updated.v1` events.

```json
{
  "sku_id": "...",
  "tax_class": "za-standard"  // tenant-defined identifier
}
```

### Multiple Taxes per Line

A SKU may be subject to **more than one tax** (e.g., VAT plus a sugar tax, VAT plus an excise duty). The tax class resolves to **a list of tax rules**, each applied in sequence:

```yaml
# Tenant tax configuration (illustrative)
tax_classes:
  za-standard:
    rules:
      - name: "VAT"
        rate: 0.15
        base: "line_total"  # applied to the pre-tax line total
      
  za-sugar-beverage:
    rules:
      - name: "Sugar Tax"
        rate: 0.0211         # per gram or per unit — depends on rule
        base: "unit_quantity"
      - name: "VAT"
        rate: 0.15
        base: "line_total_including_sugar_tax"
```

Each rule in the list is **named and auditable** — the pricing result includes each tax applied so invoices and reports can break them out.

### Jurisdiction Resolution

The tax evaluator uses the **delivery address `country_code`** (and future: state/province) to select the applicable jurisdiction's rules.

- If the address is in the tenant's home jurisdiction: standard rules apply
- If the address is in a different country: tenant may configure cross-border rules (e.g., ZA export zero-rating) or default to no tax (customer handles import duties)
- If no address is yet provided (e.g., pre-checkout product display): the tenant's **default display jurisdiction** is used, which should match the tenant's home market

### Tax-Inclusive vs Tax-Exclusive Pricing

South African retail convention is **tax-inclusive** pricing (the shelf price includes VAT). Many other markets price tax-exclusive. Tenant configures:

- `pricing.tax_inclusive = true|false`
- When `true`, the base price in YC-Pricing is **gross** (tax included), and the evaluator computes the tax portion by extraction
- When `false`, the base price is **net** (tax excluded), and tax is added

Both modes produce the same pricing audit trail — just computed differently.

## Pricing Audit Trail

Every pricing result includes:
```json
{
  "line_items": [
    {
      "sku_id": "...",
      "quantity": 2,
      "base_price": 10000,
      "subtotal": 20000,
      "taxes": [
        { "name": "VAT", "rate": 0.15, "amount": 2608, "class": "za-standard" }
      ],
      "line_total": 20000  // tax-inclusive example
    }
  ],
  "totals": {
    "subtotal_excl_tax": 17392,
    "tax_total": 2608,
    "grand_total": 20000,
    "tax_breakdown": [
      { "name": "VAT", "amount": 2608 }
    ]
  }
}
```

This structure is snapshotted on the Checkout session and the Order, so invoices and refunds can accurately break out tax.

## Consequences

**Positive**
- Flexible enough for VAT, excise duties, sugar tax, and compound taxes
- Clear audit trail per tax, per line
- SKU-level tax classification keeps tax config out of the product domain (PII-free Product stays clean)
- Easy to extract to a dedicated YC-Tax service later — the evaluator interface stays the same

**Negative**
- Tax configuration is now a Pricing concern that shop admins must understand
- Cross-border tax (e.g., EU VAT MOSS) is **not fully modeled** in v1 — tenant must handle manually or defer international sales
- No integration with external tax services (Avalara, TaxJar) in v1

## v1 Scope Explicit Non-Goals

- Automatic tax jurisdiction lookup by full address (we use `country_code` only)
- EU VAT distance selling thresholds
- US sales tax nexus rules
- Tax exemption certificates (B2B)
- Reverse charge mechanism

These can be added as more sophisticated evaluators later without changing the overall structure.
