#!/bin/bash
# Create one database and app user per service (demo). Password comes from
# POSTGRES_APP_PASSWORD in the postgres container environment (ADR-031).
set -euo pipefail

PASS="${POSTGRES_APP_PASSWORD:?POSTGRES_APP_PASSWORD must be set}"

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
	yc_tenant
	yc_support
)

# Escape single quotes for SQL string literals.
sql_pass=${PASS//\'/\'\'}

for svc in "${SERVICES[@]}"; do
	echo "Creating database and user for $svc..."
	psql -v ON_ERROR_STOP=1 --username postgres <<-EOSQL
		CREATE USER ${svc}_app WITH PASSWORD '${sql_pass}';
		CREATE DATABASE ${svc} OWNER ${svc}_app;
		GRANT ALL PRIVILEGES ON DATABASE ${svc} TO ${svc}_app;
EOSQL

	psql -v ON_ERROR_STOP=1 --username postgres --dbname "${svc}" <<-EOSQL
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
EOSQL
done

echo "All service databases created"
