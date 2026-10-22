# Move supplier management to shop admin

## Overview
**Status:** Implemented
**GitHub Issue:** #19
**Priority:** High
**Repository:** yc-platform only

## Requirement
Move every supplier endpoint off `/sysadmin/suppliers` onto `/admin/suppliers`. A shop admin maintains the suppliers of their own shop. Suppliers stay tenant-scoped; this change does not alter the table or the events.

| Method | From | To | Auth after |
|---|---|---|---|
| GET | `/sysadmin/suppliers` | `/admin/suppliers` | shop_admin |
| GET | `/sysadmin/suppliers/{id}` | `/admin/suppliers/{id}` | shop_admin |
| POST | `/sysadmin/suppliers` | `/admin/suppliers` | shop_admin |
| PUT | `/sysadmin/suppliers/{id}` | `/admin/suppliers/{id}` | shop_admin |
| DELETE | `/sysadmin/suppliers/{id}` | `/admin/suppliers/{id}` | shop_admin |

"Move" means `/sysadmin/suppliers` is removed. The old paths are not kept alongside the new ones.

The new endpoints follow the existing `/admin/products` and `/admin/categories` rules:

- The caller must be `shop_admin`. A system admin's `_platform` token is forbidden, as it already is on other `/admin/` routes.
- The shop is the tenant on the caller's token. Create does not take `tenant_id`. The list does not take `tenant_id` either: it returns that shop only, and it no longer lists every shop.
- List and get return the full supplier record (contact, status, timestamps), the same fields the system-admin reads return today. List stays newest first, with the same cursor pagination.
- A supplier another shop owns is not visible to the caller (row security), so get, update and delete of another shop's supplier are not found.
- Delete still returns 409 when any product references the supplier, deleted products included.
- The same events are published in the shop's tenant: `product.supplier.created.v1`, `product.supplier.updated.v1`, `product.supplier.deleted.v1`.

## Unchanged
- `GET /suppliers` stays public and still returns active suppliers of the shop, id and name only.
- No change to yc-platform-shop. That storefront will be rebuilt separately.

## Outcome in yc-platform
1. Code, OpenAPI, gateway route registration and tests match the table above. Nothing registers `/sysadmin/suppliers`.
2. Documentation matches the code, including `docs/services/yc-product.md`. ADR-020 rule 5 stands; its example is `POST /sysadmin/invitations`, which still names a shop. No new ADR: tenancy is unchanged, and shop-admin catalog routes already exist.

## After yc-platform is done
Log these issues. Do not change those repositories in this work.

1. **yc-platform-admin** — [issue 7](https://github.com/slaghuis/yc-platform-admin/issues/7): point supplier list, get, create, update and delete at `/admin/suppliers`.
2. **yc-platform-mcp** — [issue 1](https://github.com/slaghuis/yc-platform-mcp/issues/1): correct `specs/yc-product.yaml` for these paths.

## Acceptance
- A shop admin can list, get, create, update and delete suppliers in their own shop, including inactive ones and contact details.
- A shop admin cannot do so through `/sysadmin/suppliers`, and cannot read or write a supplier in another shop.
- A system admin cannot list, get, create, update or delete a supplier.
- A supplier referenced by a product cannot be deleted.
- The service doc, ADR-020's example and the OpenAPI spec describe the new paths.
