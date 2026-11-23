# Demo bootstrap (HostAfrica)

Run after the first successful `./deploy/demo/deploy.sh` and DNS/TLS are live. Full context: `docs/runbooks/demo-environment.md` and `docs/runbooks/onboarding-tenant.md`.

## 1. System admin

From a host that can reach the gateway **internal** listener via the Compose network (or `docker exec` into a toolbox container):

```bash
# Password from env or stdin (never argv)
export YC_BOOTSTRAP_ADMIN_PASSWORD='…'
docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD yc-identity \
  /app/service bootstrap-admin --email=ops@yellowcommerce.co.za
```

Sign in on `https://yellowcommerce.co.za` and keep a system-admin bearer for the CLI.

## 2. Provision `yc-demo`

```bash
export YC_TENANT_CLI_TOKEN=…   # system admin access token
export YC_GATEWAY_URL=https://yellowcommerce.co.za

go run ./tools/yc-tenant-cli provision \
  --tenant-id=yc-demo \
  --name="Wisefood" \
  --hostname=demo.yellowcommerce.co.za \
  --contact-email=owner@example.com \
  --primary-admin-email=owner@example.com \
  --store-currency=ZAR \
  --default-jurisdiction=ZA \
  --tax-inclusive=true
```

The owner accepts the invite at `https://demo.yellowcommerce.co.za/account/accept-invite?token=…`. Until transactional mail is configured, open MailHog over an SSH tunnel (`localhost:8025` → `yc-mailhog:8025`) and copy the link.

## 3. Payments (test mode)

Webhook URLs (ADR-031):

- Stripe: `https://demo.yellowcommerce.co.za/webhooks/stripe`
- Yoco: `https://demo.yellowcommerce.co.za/webhooks/yoco`

Register those in the Stripe/Yoco dashboards, then as the shop admin:

- `PUT /admin/gateway-config/stripe` with `sk_test_…` and the `whsec_…` secret
- Yoco: follow `docs/runbooks/yoco-setup.md` with `YOCO_WEBHOOK_URL` above
- `POST /admin/gateway-config/{stripe|yoco}/test`

## 4. Catalogue

Seed from a **checkout of `yc-platform-shop`** (not this repo’s `/opt/yc-platform` tree). Typical path on the VPS: `/opt/yc-platform-shop`.

**Do not** point `YC_GATEWAY_URL` at `https://demo.yellowcommerce.co.za`. That hostname is the Next.js shop (Caddy only sends `/webhooks/*` to yc-gateway). Catalogue seed must call the gateway’s Compose listener with the shop `Host` header:

| Variable | Value on the VPS |
|----------|------------------|
| `YC_GATEWAY_URL` | `http://yc-gateway:8080` |
| `YC_SHOP_HOST` | `demo.yellowcommerce.co.za` |

Join the demo Compose network (name from `docker network ls`, often `yc-platform_default`):

```bash
cd /opt/yc-platform-shop
NET=$(docker inspect yc-gateway -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}')

docker run --rm --network "$NET" \
  -v "$PWD":/app -w /app \
  -e YC_GATEWAY_URL=http://yc-gateway:8080 \
  -e YC_SHOP_HOST=demo.yellowcommerce.co.za \
  -e SHOP_ADMIN_EMAIL='…' \
  -e SHOP_ADMIN_PASSWORD='…' \
  node:24-bookworm \
  bash -lc 'corepack enable && corepack prepare pnpm@latest --activate && pnpm install && pnpm seed:demo'
```

`SHOP_ADMIN_*` must be a **shop admin** for `yc-demo` (invite accepted on the demo host), not only a system admin on `yellowcommerce.co.za`.

Confirm a product page loads on `https://demo.yellowcommerce.co.za`. Product images need `images.yellowcommerce.co.za` healthy; the storefront logo is baked into the shop image (`public/brand/`), not the seed.

## 5. Smoke

- [ ] `https://yellowcommerce.co.za` and `https://www.yellowcommerce.co.za` serve the console
- [ ] `https://demo.yellowcommerce.co.za` serves the shop
- [ ] Test-mode Yoco (or Stripe) checkout reaches `COMPLETED`; webhook arrives; confirmation email appears in MailHog
- [ ] `images.yellowcommerce.co.za` serves an uploaded object anonymously
