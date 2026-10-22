# Banners for a shop

## Overview
**Status:** Implemented
**GitHub Issue:** #20
**Priority:** Medium
**Repository:** yc-platform only. yc-product only.

## Requirement
A shop admin curates banners. A banner is a presentation of one existing category, not extra fields on that category. The homepage loads the banners in one call. `GET /categories` stays the navigation tree and its icon.

A banner has:

- the category it promotes
- a short text, at most 280 characters
- a background image id
- a large image id, which is not the category icon

The category name comes from the category. Both images are yc-image ids. The client resolves each URL with `GET /images/{id}`. yc-product does not check that the file still exists. yc-image does not change.

A shop has at most one banner per category. New banners are placed last. `PUT /admin/banners/order` lists every banner id of the shop, once, in the new order. Rotating, sliding, or listing them is how the storefront draws that set.

Clicking a banner opens the existing faceted search for that category, `GET /products?category={id}`, which includes its subcategories. The banner does not keep its own product list.

Deleting a category deletes its banner. `GET /banners` therefore does not show it.

## API
| Method | Path | Who |
|---|---|---|
| GET | `/banners` | anyone in the shop |
| POST | `/admin/banners` | shop_admin |
| PUT | `/admin/banners/{id}` | shop_admin. Replaces category, text and both images. Order stays. |
| PUT | `/admin/banners/order` | shop_admin |
| DELETE | `/admin/banners/{id}` | shop_admin |

`GET /banners` returns `banner_id`, `category_id`, `category_name`, `text`, `background_image_id`, `large_image_id` and `sort_order`. A system admin receives 403 on the shop-admin routes. A banner in another shop is not found.

## Out of scope
- Storing the banner text, background, or large image on the category row. Issue #17 is only the category icon.
- A new service.
- Any change to yc-image.
- yc-platform-shop.

## Follow-up
- [yc-platform-admin #9](https://github.com/slaghuis/yc-platform-admin/issues/9): the console has no banner editor, so a shop admin cannot curate banners there yet.
- [yc-platform-mcp #1](https://github.com/slaghuis/yc-platform-mcp/issues/1) stays one spec refresh. The platform OpenAPI spec now includes this contract and the category icon, so that refresh covers both.

## Test plan
`go test -tags=integration -count=1 ./...` in `services/yc-product`, plus `golangci-lint run --build-tags=integration ./...`.

`TestBannerInputValidate` checks a trimmed text, a missing text, bad ids, a text over 280 characters, and a repeated id in a reorder list.

`TestProduct/shop_admin_curates_banners` checks:

- a visitor and a system admin receive 403
- create, one banner per category, unknown category, and a large image equal to the category icon
- `GET /banners` returns the shop's order and is empty for another shop
- `GET /categories` does not include banner fields
- replace keeps the banner's place and reads the category's current name
- reorder, including a list that omits a banner
- another shop's update or delete is not found
- deleting the category removes the banner and publishes `product.banner.deleted.v1`

## Acceptance
- A shop admin can add, reorder, replace and remove banners for their shop.
- One public read returns the ordered banners with category id, category name, text, and the two image ids.
- `GET /categories` does not include banner fields.
- A banner whose category was deleted is not shown.
