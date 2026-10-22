# Rotate Secrets (Demo and Beyond)

Rotate credentials that protect tokens, service-to-service calls, field encryption and frontend sessions. Demo files live only on the VM (`deploy/demo/.env.demo`, `deploy/demo/keys/`); never commit them.

## Inventory

| Secret | Where used | Typical generation |
|--------|------------|--------------------|
| JWT RSA private/public | yc-identity; gateway validates with public | `openssl genrsa` / `openssl rsa -pubout` |
| `YC_*_SERVICE_SECRET` | ADR-021 callers + yc-identity | `openssl rand -base64 32` (≥32 chars) |
| `YC_PAYMENT_CIPHER_KEY` | yc-payment field encryption | `openssl rand -base64 32` (32 raw bytes, base64) |
| `YC_MESSAGE_CIPHER_KEY`, `YC_SHIPPING_CIPHER_KEY` | message/shipping field encryption | ≥32 character random string |
| `ADMIN_SESSION_SECRET`, `SHOP_SESSION_SECRET` | Frontend sealed cookies (ADR-027) | `openssl rand -base64 48` |
| `POSTGRES_PASSWORD`, `POSTGRES_APP_PASSWORD` | Postgres superuser / app roles | `openssl rand -base64 24` |
| SeaweedFS access/secret | S3 API + `S3_*` env | Match `seaweedfs-s3.json` |
| Stripe/Yoco shop keys + webhook secrets | Per-tenant via admin API | Dashboard rotate + `PUT /admin/gateway-config/…` |
| OAuth client secrets | yc-identity | Provider console |

## JWT signing keys

1. Generate a new key pair beside the live ones (do not overwrite yet):
   ```bash
   openssl genrsa -out deploy/demo/keys/jwt-private.next.pem 2048
   openssl rsa -in deploy/demo/keys/jwt-private.next.pem -pubout -out deploy/demo/keys/jwt-public.next.pem
   ```
2. yc-identity signs with the private key; verification uses the public key embedded/distributed with that release. **Rotating JWT keys invalidates all outstanding access and refresh tokens.** Plan a short maintenance window or accept a forced re-login for every user.
3. Stop yc-identity (and anything that caches the public key if applicable), replace:
   ```bash
   mv deploy/demo/keys/jwt-private.next.pem deploy/demo/keys/jwt-private.pem
   mv deploy/demo/keys/jwt-public.next.pem deploy/demo/keys/jwt-public.pem
   chmod 600 deploy/demo/keys/jwt-private.pem
   ```
4. `docker compose … up -d yc-identity yc-gateway` (gateway may need a bounce if it loaded JWKS/public material at start).
5. Smoke: platform and shop login; accept that existing sessions fail until users sign in again.
6. Securely destroy the previous private key.

## Service client secrets

1. Generate a new value for one secret (example cart): `openssl rand -base64 32`
2. Update **both** sides in `.env.demo` (`YC_CART_SERVICE_SECRET` is read by yc-identity and yc-cart).
3. Recreate both containers so they pick up the env:
   ```bash
   docker compose --env-file deploy/demo/.env.demo \
     -f docker-compose.yml -f deploy/demo/compose.demo.yml \
     --profile app up -d --force-recreate yc-identity yc-cart
   ```
4. Smoke an internal call path that uses that secret (e.g. cart merge / service token). Repeat per secret; avoid rotating all at once without a checklist.

## Payment / message / shipping cipher keys

**Warning:** changing `YC_PAYMENT_CIPHER_KEY` (or message/shipping cipher keys) makes previously encrypted rows unreadable unless you re-encrypt with a dual-read procedure (not built). Prefer rotating only on a rebuild of the demo environment, or accept data loss for encrypted columns.

1. Generate the new key; update `.env.demo`.
2. Recreate the owning service container.
3. For demo announce: document that historical encrypted payment credentials must be re-entered by the shop admin (`PUT /admin/gateway-config/…`).

## Frontend session secrets

1. Generate new `ADMIN_SESSION_SECRET` / `SHOP_SESSION_SECRET`.
2. Recreate `yc-platform-admin` / `yc-platform-shop`.
3. All browser sessions for that app are invalidated (users sign in again). Expected.

## Postgres passwords

App role passwords are set only by `deploy/demo/postgres-init.sh` on **first** volume init. To rotate later:

1. `ALTER USER … PASSWORD` inside Postgres for each `*_app` role (and optionally the `postgres` superuser).
2. Update `POSTGRES_APP_PASSWORD` / `POSTGRES_PASSWORD` in `.env.demo`.
3. Recreate every service that embeds the DB URL (or `compose up -d --force-recreate` the app profile).

## Object storage keys

1. Write new keys into `deploy/demo/seaweedfs-s3.json` and `.env.demo` (`S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`).
2. Recreate `seaweedfs` and `yc-image`. Existing objects remain; only credentials change.
3. Confirm `S3_PUBLIC_BASE_URL` still serves anonymous reads.

## Payment gateway webhook secrets

1. Rotate in Stripe/Yoco dashboard; keep the old endpoint until the new secret is stored.
2. Shop admin: `PUT /admin/gateway-config/{stripe|yoco}` with the new webhook secret; `POST …/test`.
3. Confirm a test event hits `https://demo.yellowcommerce.co.za/webhooks/{gateway}`.

## After any rotation

- [ ] Record what changed and when (operator notes; not in git)
- [ ] Run smoke: login platform, login shop, one test checkout webhook
- [ ] Ensure `.env.demo` mode remains `600` and is backed up with the same care as the DB (or reconstructed from a sealed store)

## Related

- [`demo-environment.md`](demo-environment.md)
- ADR-021 (service tokens), ADR-027 (session cookies), ADR-031 (demo secrets on the VM)
