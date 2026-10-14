# yc-image

`yc-image` accepts image uploads, validates them, resizes to a standard set of web-usable sizes, stores them in object storage, and makes them retrievable via stable URLs.

See `docs/services/yc-image.md` for the full requirement spec.

## Local development

```bash
task run SERVICE=yc-image
```

Database: `yc_image` (`YC_IMAGE_DB_URL`).
