#!/bin/bash
# Create one database and app user per service.
set -e

SERVICES=(
    yc_identity
    yc_image
    yc_preference
    yc_product
    yc_pricing
    yc_inventory
    yc_cart
    yc_review
    yc_checkout
    yc_payment
    yc_shipping
    yc_orders
    yc_message
    yc_audit
    yc_monitor
    yc_analytics
    yc_gateway
)

for svc in "${SERVICES[@]}"; do
    echo "Creating database and user for $svc..."
    psql -v ON_ERROR_STOP=1 --username postgres <<-EOSQL
        CREATE USER ${svc}_app WITH PASSWORD 'devpass';
        CREATE DATABASE ${svc} OWNER ${svc}_app;
        GRANT ALL PRIVILEGES ON DATABASE ${svc} TO ${svc}_app;
EOSQL

    # Enable pgcrypto for gen_random_uuid() (if not present via Postgres 13+)
    psql -v ON_ERROR_STOP=1 --username postgres --dbname "${svc}" <<-EOSQL
        CREATE EXTENSION IF NOT EXISTS pgcrypto;
EOSQL

done

echo "✓ All service databases created"
