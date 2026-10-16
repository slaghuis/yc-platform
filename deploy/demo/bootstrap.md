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
  --name="Yellow Commerce Demo" \
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

Seed the demo catalogue from yc-platform-shop's seed/demo script (see that repo). Confirm a product page loads on `https://demo.yellowcommerce.co.za`.

## 5. Smoke

- [ ] `https://yellowcommerce.co.za` and `https://www.yellowcommerce.co.za` serve the console
- [ ] `https://demo.yellowcommerce.co.za` serves the shop
- [ ] Test-mode Yoco (or Stripe) checkout reaches `COMPLETED`; webhook arrives; confirmation email appears in MailHog
- [ ] `images.yellowcommerce.co.za` serves an uploaded object anonymously
