# ADR-014: Single Store Currency, Display Currency Conversion

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Multi-currency can mean many things. We need to decide what we support.

## Decision

Each tenant has **one store currency** (the currency in which all prices are authoritatively defined and all transactions settle). Customers may view prices in a **display currency** of their choice, converted from the store currency.

**All financial truth — orders, payments, invoices, taxes, refunds — is in the store currency.** Display currency is cosmetic.

## Rules

1. **Store currency** is set at tenant provisioning. Changing it after transactions exist is **not supported** in v1 (would require a complex migration — out of scope).
2. **Display currency** is set per-user in YC-Preference. Default: store currency.
3. **Pricing pipeline** operates in store currency. Conversion to display currency is the **final evaluator** in the pricing pipeline, applied only for display.
4. **Checkout, Orders, Payment** all operate exclusively in **store currency**. The gateway charges in store currency.
5. The user is **always shown the store currency amount at the confirm step** alongside their display currency equivalent, so there is no surprise about what is being charged.
6. **Currency conversion rates** are fetched from an external rate provider (e.g., openexchangerates.org, exchangerate.host), cached in YC-Pricing with a configurable TTL (default: 1 hour).
7. Rates are **not stored on orders** — orders carry store currency only. If a customer wants to see their old order in display currency, it's a live display-time conversion.

## Supported Currencies

- **Store currencies**: the tenant may choose any currency supported by **at least one** of their enabled payment gateways. For Yoco, this is **ZAR only**. For Stripe, many currencies.
- **Display currencies**: any currency with a rate from the rate provider.

## Consequences

**Positive**
- Dramatically simpler financial logic — one currency for everything that matters
- Reconciliation is straightforward
- Tax compliance is in the tenant's local currency
- No FX risk on orders

**Negative**
- A tenant can't accept payments in multiple currencies in v1
- Display conversion is approximate — the user doesn't lock in a rate

## Future Evolution

- Multi-currency acceptance (true multi-currency orders) is a significant redesign — deferred until genuinely needed
- Rate locking on cart could be added if display-vs-charge gap becomes a UX issue
