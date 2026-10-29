#!/usr/bin/env bash
# Pull tagged images and (re)start the demo stack on the HostAfrica VM (ADR-031).
# Run from the repository root as the deploy user.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

ENV_FILE="${ENV_FILE:-deploy/demo/.env.demo}"
COMPOSE=(docker compose --env-file "$ENV_FILE" -f docker-compose.yml -f deploy/demo/compose.demo.yml)

if [[ ! -f "$ENV_FILE" ]]; then
	echo "missing $ENV_FILE — copy deploy/demo/.env.demo.example and fill secrets" >&2
	exit 1
fi

S3_JSON="${YC_DEMO_SEAWEEDFS_S3_JSON:-deploy/demo/seaweedfs-s3.json}"
if [[ ! -f "$S3_JSON" ]]; then
	echo "missing $S3_JSON — copy deploy/demo/seaweedfs-s3.json.example and set keys" >&2
	exit 1
fi

KEYS_DIR="${YC_DEMO_KEYS_DIR:-deploy/demo/keys}"
if [[ ! -f "${KEYS_DIR}/jwt-private.pem" ]]; then
	echo "missing JWT private key under ${KEYS_DIR}/" >&2
	exit 1
fi
# Containers run as non-root: dirs must be traversable and the PEM readable.
chmod 755 deploy deploy/demo "$KEYS_DIR" 2>/dev/null || true
chmod 644 "${KEYS_DIR}/jwt-private.pem" "${KEYS_DIR}/jwt-public.pem" 2>/dev/null || true
chmod 755 deploy/demo/postgres-init.sh

# shellcheck disable=SC1090
set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a

: "${VERSION:?VERSION must be set in $ENV_FILE}"
: "${REGISTRY:?REGISTRY must be set in $ENV_FILE}"

echo "==> logging in to GHCR (if needed)"
if [[ -n "${GHCR_TOKEN:-}" ]]; then
	echo "$GHCR_TOKEN" | docker login ghcr.io -u "${GHCR_USER:-$(whoami)}" --password-stdin
fi

SERVICES=(
	yc-identity yc-gateway yc-image yc-preference yc-product yc-pricing
	yc-inventory yc-cart yc-review yc-checkout yc-payment yc-shipping
	yc-orders yc-message yc-audit yc-monitor yc-analytics yc-tenant
	yc-support
)

echo "==> pulling platform images @ ${VERSION}"
for s in "${SERVICES[@]}"; do
	docker pull "${REGISTRY}/${s}:${VERSION}"
done

ADMIN_IMAGE="${ADMIN_IMAGE:-ghcr.io/slaghuis/yc-platform-admin}"
SHOP_IMAGE="${SHOP_IMAGE:-ghcr.io/slaghuis/yc-platform-shop}"
ADMIN_TAG="${ADMIN_VERSION:-$VERSION}"
SHOP_TAG="${SHOP_VERSION:-$VERSION}"

echo "==> pulling frontend images"
docker pull "${ADMIN_IMAGE}:${ADMIN_TAG}" || echo "warn: admin image ${ADMIN_IMAGE}:${ADMIN_TAG} not pulled" >&2
docker pull "${SHOP_IMAGE}:${SHOP_TAG}" || echo "warn: shop image ${SHOP_IMAGE}:${SHOP_TAG} not pulled" >&2

echo "==> compose up"
"${COMPOSE[@]}" --profile app --profile frontends up -d --remove-orphans

echo "==> status"
"${COMPOSE[@]}" --profile app --profile frontends ps
echo "deploy complete (VERSION=${VERSION})"
