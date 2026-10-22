#!/usr/bin/env bash
# Print a filled-in secret block for .env.demo (does not write the file).
set -euo pipefail

rand() { openssl rand -base64 "$1" | tr -d '\n'; }

echo "# Generated $(date -u +%Y-%m-%dT%H:%M:%SZ) — paste into deploy/demo/.env.demo"
echo "POSTGRES_PASSWORD=$(rand 24)"
echo "POSTGRES_APP_PASSWORD=$(rand 24)"
echo "YC_CART_SERVICE_SECRET=$(rand 32)"
echo "YC_ORDERS_SERVICE_SECRET=$(rand 32)"
echo "YC_CHECKOUT_SERVICE_SECRET=$(rand 32)"
echo "YC_IDENTITY_SERVICE_SECRET=$(rand 32)"
echo "YC_MESSAGE_SERVICE_SECRET=$(rand 32)"
echo "YC_PREFERENCE_SERVICE_SECRET=$(rand 32)"
echo "YC_REVIEW_SERVICE_SECRET=$(rand 32)"
echo "YC_SHIPPING_SERVICE_SECRET=$(rand 32)"
echo "YC_PAYMENT_CIPHER_KEY=$(openssl rand -base64 32 | tr -d '\n')"
echo "YC_MESSAGE_CIPHER_KEY=$(rand 32)"
echo "YC_SHIPPING_CIPHER_KEY=$(rand 32)"
echo "ADMIN_SESSION_SECRET=$(rand 48)"
echo "SHOP_SESSION_SECRET=$(rand 48)"
echo "S3_ACCESS_KEY_ID=$(openssl rand -hex 16)"
echo "S3_SECRET_ACCESS_KEY=$(openssl rand -hex 32)"
