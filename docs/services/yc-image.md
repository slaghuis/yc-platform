# yc-image — Service Requirement

## Overview

`yc-image` accepts image uploads, validates them, resizes to a standard set of web-usable sizes, stores them in object storage, and makes them retrievable via stable URLs. It is a utility service used primarily by yc-product for product imagery but is domain-agnostic.

## Location in Monorepo

```
/services/yc-image/
```

## Responsibilities

1. Accept image uploads (multipart or presigned direct-to-storage)
2. Validate uploaded images (format, size, dimensions)
3. Strip EXIF metadata (privacy and size)
4. Generate a standard set of resized variants
5. Store originals and variants in object storage
6. Serve images via stable, cacheable URLs (or CDN-fronted URLs)
7. Track image metadata (owner, upload context, variant list)
8. Delete images on request (and cascade GDPR deletions)
9. Emit lifecycle events

## Multi-Tenancy

- All images are tenant-scoped
- Object storage layout: `s3://yc-images/<tenant_id>/<image_id>/<variant>.<ext>`
- Metadata rows carry `tenant_id`
- Tenants cannot access other tenants' images (enforced by signed URLs or path-based ACLs)

## Image Variants

Standard set generated for every upload:

| Variant | Max Dimensions | Format | Use |
|---|---|---|---|
| `original` | — | preserved (JPEG/PNG/WebP) | Archive; shop admin download |
| `xlarge` | 2048×2048 | WebP + JPEG | Zoom view |
| `large` | 1024×1024 | WebP + JPEG | Product page |
| `medium` | 512×512 | WebP + JPEG | Grid listings |
| `thumbnail` | 200×200 | WebP + JPEG | Carts, order history |

- Dual format output (WebP + JPEG) for browser compatibility
- Aspect ratio preserved — these are maximums, not forced crops
- Variant list is **tenant-configurable** in future scope; v1 ships with this fixed set

## Input Validation

- **Allowed formats**: JPEG, PNG, WebP, HEIC (converted to JPEG), GIF (first frame only in v1)
- **Max upload size**: 20 MB (configurable)
- **Max dimensions**: 10000×10000 (configurable)
- **Reject on**: corrupt files, embedded scripts (SVG not allowed in v1), unreasonable aspect ratios (configurable)
- **Virus/malware scan**: deferred to future scope; v1 relies on format validation only

## Upload Flow

### Direct Upload (small images)

```
Client → yc-gateway → yc-image: POST /images (multipart)
  yc-image:
    1. Validate
    2. Store original in staging
    3. Create image record (status: processing)
    4. Return image_id + status URL
    5. Spawn async processing: strip EXIF, generate variants, store, publish event
```

### Presigned Upload (large images, preferred)

```
Client → yc-image: POST /images/upload-url  → { upload_url, image_id }
Client → ObjectStorage: PUT upload_url (direct)
Client → yc-image: POST /images/{image_id}/finalize
  yc-image: fetch from storage, validate, process variants
```

Direct upload is simpler for v1 but presigned upload avoids streaming large files through the service. **v1 implements both.**

## API

### Public / Authenticated Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/images` | Direct upload (multipart) | shop_admin / system_admin |
| POST | `/images/upload-url` | Request presigned URL | shop_admin / system_admin |
| POST | `/images/{id}/finalize` | Finalize presigned upload | shop_admin / system_admin |
| GET | `/images/{id}` | Get image metadata | any (image visibility depends on context) |
| GET | `/images/{id}/variants/{variant}` | Redirect to variant URL | any |
| DELETE | `/images/{id}` | Delete image | shop_admin (own tenant) / system_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/images/bulk-delete` | Bulk delete by owner reference | service |

## Database Schema

Database: **`yc_image`**

```sql
CREATE TABLE images (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    uploader_id     TEXT NOT NULL,          -- user who uploaded
    status          TEXT NOT NULL,          -- 'pending','processing','ready','failed','deleted'
    original_format TEXT,
    width           INT,
    height          INT,
    bytes           BIGINT,
    checksum_sha256 TEXT,
    owner_ref       TEXT,                   -- optional: "product:<id>", "review:<id>", etc.
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX idx_images_tenant ON images (tenant_id);
CREATE INDEX idx_images_owner_ref ON images (tenant_id, owner_ref);

CREATE TABLE image_variants (
    image_id        UUID NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    variant         TEXT NOT NULL,          -- 'original','xlarge','large','medium','thumbnail'
    format          TEXT NOT NULL,          -- 'webp','jpeg','png'
    width           INT,
    height          INT,
    bytes           BIGINT,
    storage_key     TEXT NOT NULL,
    cdn_url         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (image_id, variant, format)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE images ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON images
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
```

## Object Storage

- **v1 target**: S3-compatible storage (AWS S3, MinIO for dev, DigitalOcean Spaces, Backblaze B2)
- **Storage layout**: `/<tenant_id>/<image_id>/<variant>.<ext>`
- **Access**: public-read for product images via CDN (acceptable since no PII in product images); signed URLs reserved for future private image scenarios

## CDN

- **v1**: Cloudflare or Bunny.net in front of object storage
- CDN URL stored in `image_variants.cdn_url` once uploaded; this is what consumers use
- CDN purge on image deletion

## Published Events

| Event | When |
|---|---|
| `image.uploaded.v1` | Upload accepted, processing started |
| `image.processed.v1` | All variants ready; CDN URLs populated |
| `image.processing_failed.v1` | Validation or processing failed |
| `image.deleted.v1` | Image deleted (triggers downstream cleanup) |

## Subscribed Events

| Event | Action |
|---|---|
| `product.deleted.v1` | Delete images with `owner_ref = "product:<id>"` |
| `product.sku.deleted.v1` | Delete images with `owner_ref = "sku:<id>"` |
| `review.rejected.v1` | Delete images attached to the review (if any) |
| `platform.tenant.deleted.v1` | Delete all images for the tenant |
| `identity.user.deleted.v1` | No action (images are tenant-scoped, not user-scoped; PII is not stored in images beyond EXIF which is stripped) |

## Configuration

```yaml
yc-image:
  listen: :8080
  database:
    url: postgres://yc_image:...@...
  storage:
    provider: s3
    bucket: yc-images
    region: eu-west-1
    endpoint: ...                     # for S3-compatible
    access_key: ...
    secret_key: ...
  cdn:
    base_url: https://cdn.example.com
    purge_api_url: ...
    purge_api_key: ...
  limits:
    max_upload_mb: 20
    max_dimension: 10000
  variants:                            # fixed in v1
    - name: xlarge
      max_width: 2048
      max_height: 2048
    # ...
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None at startup** — self-contained
- **Depends on**: object storage, CDN (external infrastructure)

## Called By

- Clients (via gateway) for upload
- CDN consumers (indirectly) for image retrieval

## GDPR Behavior

- No PII stored in image metadata
- EXIF is stripped on ingestion (removes GPS, camera serial, etc.)
- User-generated images (e.g., review photos) are subject to the GDPR cascade via `review.rejected.v1` or explicit `review.deleted.v1`

## Open Questions / Future Scope

- Automatic focal point detection for smart cropping
- AVIF format output when browser share justifies it
- Video upload support (separate service likely)
- Virus/malware scanning
- Image moderation (NSFW detection)
- Custom variant sets per tenant
- Progressive JPEG / blur-up placeholders

