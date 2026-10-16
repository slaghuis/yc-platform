#!/usr/bin/env bash
# Validate demo Compose merge without contacting a registry or VM.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

# Minimal env so interpolation succeeds (not real secrets).
cat >"$TMP" <<'EOF'
VERSION=v1.1.0-beta.1
REGISTRY=ghcr.io/slaghuis/yc-platform
ADMIN_IMAGE=ghcr.io/slaghuis/yc-platform-admin
SHOP_IMAGE=ghcr.io/slaghuis/yc-platform-shop
YC_DEMO_ACME_EMAIL=ops@example.com
YC_GATEWAY__PLATFORM_HOSTS=yellowcommerce.co.za,www.yellowcommerce.co.za
YC_GATEWAY__TRUSTED_PROXIES=172.16.0.0/12
POSTGRES_PASSWORD=validate-only
POSTGRES_APP_PASSWORD=validate-only
YC_DEMO_KEYS_DIR=./deploy/demo/keys
YC_DEMO_SEAWEEDFS_S3_JSON=./deploy/demo/seaweedfs-s3.json.example
YC_CART_SERVICE_SECRET=validate-cart-service-secret-32chars!
YC_ORDERS_SERVICE_SECRET=validate-orders-service-secret-32ch
YC_CHECKOUT_SERVICE_SECRET=validate-checkout-service-secret-32
YC_IDENTITY_SERVICE_SECRET=validate-identity-service-secret-32
YC_MESSAGE_SERVICE_SECRET=validate-message-service-secret-32c
YC_PREFERENCE_SERVICE_SECRET=validate-preference-service-secret32
YC_REVIEW_SERVICE_SECRET=validate-review-service-secret-32ch!
YC_SHIPPING_SERVICE_SECRET=validate-shipping-service-secret-32
YC_PAYMENT_CIPHER_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
YC_MESSAGE_CIPHER_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
YC_SHIPPING_CIPHER_KEY=yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy
S3_ACCESS_KEY_ID=localdev
S3_SECRET_ACCESS_KEY=localdev
S3_PUBLIC_BASE_URL=https://images.yellowcommerce.co.za
ADMIN_SESSION_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SHOP_SESSION_SECRET=yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy
YC_MESSAGE_SYSTEM_ADMIN_EMAILS=ops@example.com
EOF

echo "==> docker compose config (demo merge)"
docker compose --env-file "$TMP" \
	-f docker-compose.yml -f deploy/demo/compose.demo.yml \
	--profile app --profile frontends config >/dev/null

echo "compose config OK"
