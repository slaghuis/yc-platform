# Customer billing address and SARS-oriented invoices

## Overview
**Status:** Approved
**GitHub Issues:** [#29](https://github.com/slaghuis/yc-platform/issues/29)
**Priority:** High (Wisefood checkout §7.2; fit-gap GAP-004 / CHK-003)
**Repository:** yc-platform — yc-identity, yc-tenant, yc-checkout, yc-orders; yc-message for delivery of the artefact. Storefront/BFF consumes OpenAPI (ADR-028).
**Work package:** WP4b (after WP4a / #22)

## Requirement

Enable South African shops to produce a **customer tax invoice** (or a non-VAT sales invoice when the shop is not a VAT vendor) by adding:

1. A **structured customer billing profile** (company, VAT number, SA postal fields).
2. An **optional store VAT number** on the shop/tenant record.
3. An **order-time snapshot** of buyer billing + seller identity, and a **documented invoice artefact** that uses existing pricing/order tax totals — **no new tax engine** ([ADR-016](../adr/ADR-016%20Tax%20within%20Pricing%20—%20Multiple%20Tax%20Rates%20per%20Product.md)).

Delivery address stays free-form JSON ([ADR-015](../adr/ADR-015%20Address%20Storage%20as%20JSON.md)). Saved delivery address books (GAP-008) and a general customer `PATCH /me` (API-Q004) remain out of scope.

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Checkout confirm | `guest_contact`, free-form `delivery_address` only — no billing |
| `GET /me` | Identity fields (+ OAuth phone/image from #27); no billing block |
| Order snapshot | Items, `pricing_snapshot`, delivery, guest contact — no billing / store VAT |
| yc-tenant | `name`, `contact_email`, currency, jurisdiction, hostnames; **no VAT**; PATCH is **sysadmin-only** (ADR-029) |
| Invoice API / PDF | None |
| Tax | Computed and snapshotted by yc-pricing / checkout / orders (ADR-016) |

### Design notes already on the issue

- Billing is **structured for SA invoicing**, not a replacement of ADR-015 delivery JSON.
- ADR-016 already snapshots tax for invoices/refunds; this package adds **party identity** and the **artefact**.
- Tax exemption certificates / reverse charge stay non-goals of ADR-016 unless explicitly expanded.

---

## Decisions proposed for this draft

| Topic | Proposal |
|---|---|
| Billing vs delivery | **Typed** `BillingAddress` schema for invoices; delivery remains ADR-015 free-form JSON. Document as an ADR-015 **amendment** (billing exception), not a rewrite of delivery. |
| Customer billing home | **yc-identity**: `GET` / `PUT /me/billing` for customers. Narrow write surface — **not** a general `PATCH /me` (API-Q004 stays separate). |
| Guests | No profile row. Billing is supplied on **checkout confirm** and snapshotted onto the order only. |
| Customers at checkout | Confirm accepts optional `billing_address`. If omitted and the customer has a saved billing profile, checkout **copies the profile** into the order snapshot. If both omitted → order has no billing snapshot (invoice may still be a simple receipt — see artefact rules). |
| When billing is required | **Not required** by the platform for every checkout. Storefronts that need SARS tax invoices (Wisefood) require it in UX. Platform validates shape when present. |
| Billing fields | See schema below. `company_name` and `vat_number` optional; when any billing object is present, street+number, town/city, and postal code are required; suburb optional. |
| VAT number format | Store as normalised string (trim, uppercase). Soft-check SA pattern `4xxxxxxxxx` (10 digits starting with 4) with a clear validation error; do **not** call SARS to verify. Same rules for customer and store. |
| Store VAT | Optional `vat_number` on **yc-tenant**. Absence is valid (non-vendor / not collecting VAT on invoices). |
| Who edits store VAT | New shop-admin surface: `GET` / `PATCH /admin/shop` on yc-tenant (own tenant only) for invoice identity fields (`vat_number`, and optionally `invoice_address`). Sysadmin PATCH may also set them. Avoid stuffing this into `_platform`-only sysadmin flows for day-to-day shops. |
| Seller address on invoice | Today tenant has no postal address. Add optional structured **`invoice_address`** (same field set as customer billing without company/VAT) on the tenant. When absent, invoice shows shop **name** + **contact_email** + optional VAT only, and docs state that a full SARS tax invoice for a VAT vendor should set `invoice_address`. |
| Order snapshot | On place/confirm: persist `billing_address` (buyer) and `seller_invoice` `{ name, contact_email, vat_number?, invoice_address? }` copied from tenant at that moment. Later profile/tenant edits must not rewrite history. |
| Invoice number | Use **`order_number`** as the invoice number (one tax invoice identity per paid order). Document; do not invent a second sequence in this package. |
| Artefact | **`GET /orders/{id}/invoice`** (customer/visitor owner or valid `guest_token`; shop admin for their shop) returns `application/pdf`. Content rules below. Optional: attach the same PDF on order-confirmation email when the store has `vat_number` **or** always attach a “invoice/receipt” PDF — **propose: generate on download first; email attachment is a follow-up** so message templates stay stable. |
| Tax amounts | Taken only from the order’s existing line/`pricing_snapshot` / tax totals. No recalculation. |
| Non-VAT shop | If store `vat_number` is empty: document titled **Invoice** (not “Tax Invoice”); still shows lines and tax breakdown from the snapshot when tax was charged under the shop’s pricing config. |
| VAT shop | Document titled **Tax Invoice**; must include store name, `invoice_address` when set, store VAT number, buyer billing when present, order number, placed date, line descriptions, consideration, and VAT breakdown from the snapshot. |
| Events | Extend or version tenant update carefully: prefer adding `vat_number` (and invoice address) on a **new** `platform.tenant.updated.v2` **or** keep v1 unchanged and have orders read tenant via internal GET at confirm time (no event needed for snapshot). **Propose: snapshot via internal read at order confirm** — no event schema change required for MVP. Shop-admin PATCH still audits via existing tenant update event only for fields that event already carries (name); VAT-only changes need either event extension or audit-only log — **propose extend `platform.tenant.updated` via a new version** if other services must display store VAT; otherwise orders/message read yc-tenant when generating the PDF. |
| GDPR | Billing on identity deleted/anonymised with the user. Order snapshots retain billing for legal retention / pseudonymisation like other order PII (ADR-005). |
| MCP / OpenAPI | Publish with the platform tag (ADR-028); call out breaking storefront contract for confirm body + new routes. |

---

## Part A — Customer billing profile (#29)

### Schema (`BillingAddress`)

| Field | Required when billing present | Notes |
|---|---|---|
| `company_name` | no | B2B / trading name on the invoice |
| `vat_number` | no | Customer VAT registration when they have one |
| `street_and_number` | **yes** | |
| `suburb` | no | |
| `town_city` | **yes** | |
| `postal_code` | **yes** | |
| `country_code` | no (default `ZA`) | ISO 3166-1 alpha-2; reserved for later multi-market billing |

Empty profile: customer has never `PUT` billing (or cleared it with `PUT` of null/empty — **propose: `DELETE /me/billing`** clears).

### API (yc-identity)

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET | `/me/billing` | customer | 200 + billing, or 404 if none |
| PUT | `/me/billing` | customer | Replace billing; validate schema |
| DELETE | `/me/billing` | customer | Remove saved billing |

Visitors: 403 on these routes (guests use checkout only).

### Checkout (yc-checkout)

Extend `ConfirmInput` with optional `billing_address` (`BillingAddress`).

- Visitor: if Wisefood (or any shop) sends it, validate and pass through to order create.
- Customer: body wins; else load identity billing when creating the order (checkout calls identity internal, or orders loads it — **propose: checkout accepts body only; yc-orders resolves customer profile billing when confirm body omits it**, via `GET /internal/users/{id}/billing` on identity, so checkout stays thin).

### Order (yc-orders)

- Persist `billing_address` JSONB (structured, validated) on the order.
- Expose on `GET /orders/{id}` and admin/internal order payloads.
- Include in GDPR export / cascade handling as order PII.

---

## Part B — Store VAT and seller invoice identity (#29)

### yc-tenant fields

| Field | Required | Notes |
|---|---|---|
| `vat_number` | no | Soft-validated SA VAT pattern when set |
| `invoice_address` | no | Structured postal fields for the seller on the tax invoice |

### API

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET | `/admin/shop` | shop_admin | Own tenant invoice identity + name, contact_email (read model) |
| PATCH | `/admin/shop` | shop_admin | Update `vat_number`, `invoice_address` (and only those in this package) |
| PATCH | `/sysadmin/tenants/{id}` | system_admin | May also set the same fields |

Sysadmin create/provision may leave VAT empty.

### Snapshot at order confirm

`seller_invoice` on the order:

```json
{
  "name": "Wisefood",
  "contact_email": "ops@…",
  "vat_number": "4123456789",
  "invoice_address": {
    "street_and_number": "…",
    "suburb": "…",
    "town_city": "…",
    "postal_code": "…",
    "country_code": "ZA"
  }
}
```

Omit nulls. Copied once; never live-joined for historical invoices.

---

## Part C — Invoice artefact (#29)

### Content (platform-authored PDF)

Minimum fields (SARS-oriented; not a substitute for legal advice):

| Block | Source |
|---|---|
| Title | `Tax Invoice` if `seller_invoice.vat_number` set; else `Invoice` |
| Invoice number | `order_number` |
| Invoice date | `placed_at` (UTC date in shop locale later; UTC date in v1) |
| Supplier | `seller_invoice` |
| Recipient | `billing_address` when present; else customer/guest email from the order (receipt-quality, not a full tax invoice recipient block) |
| Lines | Order items: name snapshot, qty, line total |
| Tax | From order tax totals / `pricing_snapshot` breakdown |
| Totals | Items, shipping, tax, grand total (store currency, ADR-014) |

### API

| Method | Path | Auth |
|---|---|---|
| GET | `/orders/{id}/invoice` | customer/visitor owner, or `guest_token`; shop_admin |

Response: `200` + `application/pdf`, `Content-Disposition: attachment; filename="{order_number}.pdf"`.

404 when the caller cannot see the order. Always available for placed orders (even without billing/VAT) so shops have a downloadable receipt; title and completeness follow the rules above.

### Email

**Out of scope for the first merge of this package:** attaching the PDF to `order_confirmation`. Storefronts and shops use download. A follow-up can add attachment once PDF generation is stable.

### Storefront URL contract

No new page required if download is API-only through the BFF. If the shop wants a page, prefer existing `/orders/{order_id}` with a download action — **no ADR-027 path change** unless we add `/orders/{id}/invoice` as a browser page (not proposed).

---

## ADR / docs impact

1. **ADR-015 amendment** — billing may be a typed schema; delivery stays free-form JSON.
2. **ADR-016** — no change to tax evaluation; cite invoice consumer of snapshots.
3. **ADR-029** — shop-admin `/admin/shop` for invoice fields; optional event version if VAT must fan out.
4. Service docs: yc-identity, yc-tenant, yc-checkout, yc-orders.
5. OpenAPI + release notes (ADR-028).

---

## Acceptance criteria

- [ ] Customer can `PUT`/`GET`/`DELETE /me/billing` with the structured fields; validation errors are field-scoped.
- [ ] Guest (and customer) checkout confirm accepts `billing_address`; order stores a snapshot.
- [ ] Customer confirm without body billing uses saved `/me/billing` when present.
- [ ] Shop admin can set/clear optional store `vat_number` and `invoice_address` via `/admin/shop`.
- [ ] Placed order exposes immutable `billing_address` and `seller_invoice` snapshots.
- [ ] `GET /orders/{id}/invoice` returns a PDF using snapshotted parties + existing tax totals; title Tax Invoice vs Invoice follows store VAT.
- [ ] Profile/tenant edits after place do not change prior invoices.
- [ ] GDPR: identity billing cleared with the user; order billing retained/pseudonymised per ADR-005.
- [ ] Delivery address behaviour unchanged (ADR-015).
- [ ] Feature spec + OpenAPI + service docs updated; no new tax calculation paths.

---

## Out of scope

- Saved **delivery** address book (GAP-008).
- General customer `PATCH /me` / rich profile editor (API-Q004).
- SARS live VAT validation, tax exemption certificates, reverse charge (ADR-016 non-goals).
- Credit notes / debit notes / multiple invoices per order.
- Email PDF attachment (follow-up).
- Multi-currency invoices (store currency only, ADR-014).
- Shop trading logo on the PDF (follow-up; can use existing image assets later).

---

## Test plan

1. Identity: PUT billing → GET; invalid VAT / missing street → 400; DELETE → 404 on GET; visitor → 403.
2. Checkout/orders: guest confirm with billing → order snapshot; customer with profile only → snapshot filled; neither → null billing.
3. Tenant: shop admin PATCH VAT; other shop → 404/403; order confirm copies seller_invoice.
4. Invoice PDF: VAT shop + billing → “Tax Invoice” + parties; non-VAT shop → “Invoice”; tax lines match order totals; after changing tenant VAT, old order PDF still shows old seller_invoice.
5. Auth: other customer 404; guest_token works for guest orders (#22).
6. GDPR delete user clears `/me/billing`; order row still readable by admin with pseudonymisation rules unchanged.

---

## Decisions locked (approval 2026-10-28)

1. **Resolve billing:** yc-orders loads `GET /internal/users/{id}/billing` when create omits billing (customer only).
2. **Email PDF:** deferred; download-only in this package.
3. **Tenant event:** no schema change; snapshot via `GET /internal/tenants/{id}` at order create.
4. **`invoice_address` when VAT set:** soft docs guidance only (not a hard 400).
5. **Country:** default `ZA`; any ISO alpha-2 accepted without further validation.

---

## Implementation sketch

1. Spec → Approved; ADR-015 amendment + ADR-029 `/admin/shop` note.
2. yc-identity billing persistence + `/me/billing` + internal get.
3. yc-tenant `vat_number` / `invoice_address` + `/admin/shop` + internal get.
4. yc-checkout confirm `billing_address`; yc-orders snapshot columns + resolve logic.
5. PDF in yc-orders + `GET /orders/{id}/invoice`.
6. Docs, OpenAPI, tests; storefront tag for contract sync.
