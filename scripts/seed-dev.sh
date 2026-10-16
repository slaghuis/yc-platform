#!/usr/bin/env bash
# Seeds the dev stack for frontend work (yc-platform v1.1 Phase 10): the
# bootstrap system admin, the yc-demo shop on yc-demo.localhost with a shop
# admin, and the image bucket's CORS rule for uploads from shop hosts.
# Safe to run again: every step checks before it acts.
#
# Usage: task devstack:seed (after task devstack:up and task devstack:app)
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
EDGE_PORT=${YC_EDGE_PORT:-8070}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
SHOP_TENANT=${SHOP_TENANT:-yc-demo}
SHOP_HOST=${SHOP_HOST:-$SHOP_TENANT.localhost}
SHOP_ADMIN_EMAIL=${SHOP_ADMIN_EMAIL:-admin@$SHOP_TENANT.test}
SHOP_ADMIN_PASSWORD=${SHOP_ADMIN_PASSWORD:-demo-shop-admin-pass}
S3_URL=${S3_URL:-http://localhost:9000}
S3_BUCKET=${S3_BUCKET:-yc-images}
S3_ACCESS_KEY_ID=${S3_ACCESS_KEY_ID:-devkey}         # deploy/compose/seaweedfs-s3.json
S3_SECRET_ACCESS_KEY=${S3_SECRET_ACCESS_KEY:-devsecret}
export NATS_URL=${NATS_URL:-nats://localhost:4222}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }

# call <host> <token|-> <method> <path> [json] sets STATUS and BODY.
call() {
    local host=$1 auth=$2 method=$3 path=$4 body=${5:-}
    local args=(-sS -o "$TMP/body" -w '%{http_code}' -X "$method" -H "Host: $host")
    [ "$auth" = - ] || args+=(-H "Authorization: Bearer $auth")
    [ -z "$body" ] || args+=(-H "Content-Type: application/json" --data "$body")
    STATUS=$(curl "${args[@]}" "$GATEWAY$path")
    BODY=$(cat "$TMP/body")
}

expect() {
    local what=${*: -1} want
    for want in "${@:1:$#-1}"; do
        [ "$STATUS" = "$want" ] && return 0
    done
    echo "$BODY" | head -c 600 | sed 's/^/    /'; echo
    fail "$what: HTTP $STATUS, want ${*:1:$#-1}"
}

poll() {
    local deadline=$((SECONDS + $1)); shift
    until "$@" >/dev/null 2>&1; do
        [ $SECONDS -lt $deadline ] || return 1
        sleep 0.5
    done
}

for tool in curl jq go openssl; do
    command -v "$tool" >/dev/null || fail "$tool is required"
done
curl -fsS "$GATEWAY/readyz" >/dev/null || fail "yc-gateway is not ready at $GATEWAY (task devstack:app)"

step "System admin $SYSADMIN_EMAIL"
docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
    /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
expect 200 "system admin login on $ADMIN_HOST"
SYS=$(jq -r .access_token <<<"$BODY")
ok "signs in on $ADMIN_HOST"

step "Shop $SHOP_TENANT on $SHOP_HOST"
shop_mapped() { [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $SHOP_HOST" "$GATEWAY/me")" = 200 ]; }
if shop_mapped; then
    ok "already provisioned"
else
    # shellcheck source=lib/provision-tenant.sh
    source scripts/lib/provision-tenant.sh
    provision_tenant "$SHOP_TENANT" "Yellow Commerce Demo" "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true >/dev/null
    poll 30 shop_mapped || fail "yc-gateway did not map $SHOP_HOST"
    ok "provisioned; the gateway answers on $SHOP_HOST"
fi

step "Shop admin $SHOP_ADMIN_EMAIL"
call "$SHOP_HOST" - POST /register "{\"email\":\"$SHOP_ADMIN_EMAIL\",\"password\":\"$SHOP_ADMIN_PASSWORD\",\"display_name\":\"Demo Shop Admin\"}"
expect 201 409 "register shop admin"
call "$ADMIN_HOST" "$SYS" GET "/admin/users?tenant_id=$SHOP_TENANT&email=$(jq -rn --arg e "$SHOP_ADMIN_EMAIL" '$e|@uri')"
expect 200 "find shop admin"
user_id=$(jq -r '.items[0].user_id // empty' <<<"$BODY")
role=$(jq -r '.items[0].role // empty' <<<"$BODY")
[ -n "$user_id" ] || fail "$SHOP_ADMIN_EMAIL not found in $SHOP_TENANT"
if [ "$role" = shop_admin ]; then
    ok "already a shop admin"
else
    call "$ADMIN_HOST" "$SYS" POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote to shop admin"
    ok "promoted to shop admin"
fi
call "$SHOP_HOST" - POST /login "{\"email\":\"$SHOP_ADMIN_EMAIL\",\"password\":\"$SHOP_ADMIN_PASSWORD\"}"
expect 200 "shop admin login on $SHOP_HOST (an existing account with another password? set SHOP_ADMIN_PASSWORD)"
ok "signs in on $SHOP_HOST"

step "Image uploads from shop hosts (bucket CORS, ADR-027 rule 15)"
cat >"$TMP/cors.xml" <<EOF
<CORSConfiguration><CORSRule><AllowedOrigin>http://*.localhost:$EDGE_PORT</AllowedOrigin><AllowedOrigin>http://localhost:3001</AllowedOrigin><AllowedMethod>PUT</AllowedMethod><AllowedHeader>*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>
EOF
STATUS=$(curl -sS -o "$TMP/body" -w '%{http_code}' -X PUT "$S3_URL/$S3_BUCKET?cors" \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "$S3_ACCESS_KEY_ID:$S3_SECRET_ACCESS_KEY" \
    -H "Content-MD5: $(openssl md5 -binary "$TMP/cors.xml" | base64)" -H 'Content-Type: application/xml' \
    --data-binary @"$TMP/cors.xml")
BODY=$(cat "$TMP/body")
expect 200 "put bucket CORS"
ok "PUT allowed from http://*.localhost:$EDGE_PORT and http://localhost:3001"

printf '\nSeeded. With the dev edge (task devstack:edge):\n'
printf '  %-8s %-34s %s\n' console "http://$ADMIN_HOST:$EDGE_PORT" "$SYSADMIN_EMAIL / $SYSADMIN_PASSWORD" \
    shop "http://$SHOP_HOST:$EDGE_PORT" "$SHOP_ADMIN_EMAIL / $SHOP_ADMIN_PASSWORD" \
    mail http://localhost:8025 ""
