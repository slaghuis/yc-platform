# Associate an image with a product category

## Overview
**Status:** Implemented
**GitHub Issue:** #17
**Priority:** Medium
**Repository:** yc-platform only. yc-product only.

## Requirement
A shop admin can set one image on a category. `GET /categories` returns that image with every category in the shop's tree, so a storefront can show an icon beside the category name.

The image is a yc-image id, the same way a product image is stored. yc-product does not store the file. The client resolves the URL with `GET /images/{id}`.

## API
`image_id` is optional on create and update. The shop admin uploads the file through yc-image first, then sends the id. Update replaces the category: a new id replaces the icon, and omitting `image_id` clears it. The id must be a UUID. yc-product does not check that yc-image still has the file.

`GET /categories` adds `image_id` to every node, including nested categories. The field is absent when the category has no image. Create and update responses include it too.

## Out of scope
- Banners. A banner is its own resource in yc-product (issue #20), not another image on the category.
- A description on the category.
- More than one image, or roles such as `gallery` and `background`.
- Any change to yc-image. Issue #18 is cancelled.
- yc-product's `is_primary` flag on product images.
- yc-platform-shop.

## Follow-up
- [yc-platform-admin #8](https://github.com/slaghuis/yc-platform-admin/issues/8): the console has no category editor, so a shop admin cannot set the icon there yet.
- [yc-platform-mcp #1](https://github.com/slaghuis/yc-platform-mcp/issues/1) stays one spec refresh. The platform OpenAPI spec now includes this icon and the banners from issue #20.

## Test plan
`go test -tags=integration -count=1 ./...` in `services/yc-product`.

`TestProduct/shop_admin_builds_a_category_tree` checks that a shop admin can set, replace and clear `image_id`, that a bad id is rejected, that a system admin receives 403, and that `GET /categories` omits `image_id` when a category has no icon and returns it on a nested category.

## Acceptance
- A shop admin can set, replace and clear a category image.
- `GET /categories` returns each category's `image_id`, and omits it when there is none.
- A category with no image still appears in the tree.
- A system admin cannot call the shop-admin category routes.
