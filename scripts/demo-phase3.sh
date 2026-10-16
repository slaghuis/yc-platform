#!/usr/bin/env bash
# Phase 3 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 3 — Cart and Authenticated Users").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase3.sh
#
# Everything goes through yc-gateway: admin.localhost is the platform host
# (system admins), $SHOP_HOST is the shop. Google sign-in uses yc-identity's
# fake provider (YC_IDENTITY_OAUTH_FAKE=true in the dev stack). The currency
# step needs yc-pricing to reach api.frankfurter.dev. Needs curl, jq, docker
# and go. Safe to re-run: each run uses fresh emails and products.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
TENANT=${TENANT:-demo-shop}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
RUN=$(date +%s)
export NATS_URL=${NATS_URL:-nats://localhost:4222}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }

# poll <seconds> <command...>: retry until the command succeeds.
poll() {
    local deadline=$((SECONDS + $1)); shift
    until "$@" >/dev/null 2>&1; do
        [ $SECONDS -lt $deadline ] || return 1
        sleep 0.5
    done
}

# call <host> <auth> <method> <path> [json] sets STATUS and BODY. auth is a
# bearer token, "jar:<file>" for a visitor's cookie jar, or "-" for none.
# Extra curl arguments may follow the body.
call() {
    local host=$1 auth=$2 method=$3 path=$4 body=${5:-}
    shift 5 2>/dev/null || shift $#
    local args=(-sS -o "$TMP/body" -w '%{http_code}' -X "$method" -H "Host: $host")
    case $auth in
        -) ;;
        jar:*) args+=(-b "${auth#jar:}" -c "${auth#jar:}") ;;
        *) args+=(-H "Authorization: Bearer $auth") ;;
    esac
    [ -z "$body" ] || args+=(-H "Content-Type: application/json" --data "$body")
    STATUS=$(curl "${args[@]}" "$@" "$GATEWAY$path")
    BODY=$(cat "$TMP/body")
}

# expect <status...> <what>: fail unless STATUS is one of the given codes.
expect() {
    local what=${*: -1} want
    for want in "${@:1:$#-1}"; do
        [ "$STATUS" = "$want" ] && return 0
    done
    echo "$BODY" | head -c 800 | sed 's/^/    /'; echo
    fail "$what: HTTP $STATUS, want ${*:1:$#-1}"
}

# Money formatting for jq: {"amount_minor": 20000, "currency": "ZAR"} → ZAR 200.00
JQ_MONEY='def amt: "\(.currency) \(.amount_minor / 100 | floor).\(.amount_minor % 100 | tostring | if length < 2 then "0" + . else . end)";'

# show_cart prints the cart in BODY.
show_cart() {
    jq -r --argjson names "$NAMES" "$JQ_MONEY"'
        (.items[] | "    \($names[.sku_id]) × \(.quantity)  snapshot \(.unit_price | amt)" +
            (if .available then "  now \(.current_unit_price | amt)  line \(.line_total | amt)" else "  unavailable" end) +
            (if .price_changed then "  ← price changed" else "" end) +
            (if .display then "  (\(.display.line_total | amt))" else "" end)),
        (if .subtotal then "    subtotal \(.subtotal | amt)" + (if .display then "  =  \(.display.subtotal | amt) at \(.display.rate)" else "" end) else "    (empty)" end)' <<<"$BODY"
}

# qty <sku> prints the quantity of a line in the cart in BODY.
qty() { jq -r --arg s "$1" '[.items[] | select(.sku_id == $s) | .quantity] | first // 0' <<<"$BODY"; }

healthy() {
    step "0. Services are up and registered with the gateway"
    for port in 8090 8081 8083 8084 8086 8087; do
        curl -fsS "http://localhost:$port/readyz" >/dev/null || fail "localhost:$port/readyz"
    done
    ok "gateway, identity, product, pricing, preference and cart are ready"
}

routed() { ! curl -s -H "Host: $SHOP_HOST" "$GATEWAY$1" | grep -q 'Route not found'; }

provision() {
    step "Provision tenant $TENANT ($SHOP_HOST, ZAR, VAT-inclusive)"
    # shellcheck source=lib/provision-tenant.sh
    source scripts/lib/provision-tenant.sh
    provision_tenant "$TENANT" "Demo Shop" "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"
    for path in /products /carts /preferences/me; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "product, cart and preference routes registered with the gateway"
}

shop_admin() {
    step "Shop admin and catalog: three products, VAT-inclusive prices"
    docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
        /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
    call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
    expect 200 "system admin login"
    local sys jar=$TMP/owner.jar email="owner+$RUN@demo-shop.test" password="owner-password-$RUN" user_id
    sys=$(jq -r .access_token <<<"$BODY")
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 201 "register owner"
    user_id=$(jq -r .user_id <<<"$BODY")
    call "$ADMIN_HOST" "$sys" POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote"
    call "$SHOP_HOST" - POST /login "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 200 "shop admin login"
    ADMIN=$(jq -r .access_token <<<"$BODY")
    ok "shop admin $email"

    call "$SHOP_HOST" "$ADMIN" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 200 "pricing config"
    call "$SHOP_HOST" "$ADMIN" PUT /admin/tax-classes/za-standard \
        '{"display_name":"Standard VAT","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0.15","base":"line_total","sort_order":0}]}'
    expect 200 201 "tax class"

    product() { # product <name> <price minor> → sets SKU
        call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"$1 $RUN\",\"description\":\"Phase 3 demo\"}"
        expect 201 "create $1"
        local id; id=$(jq -r .product_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\"}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000;  KETTLE=$SKU
    product Toaster 45000; TOASTER=$SKU
    product Blender 90000; BLENDER=$SKU
    NAMES=$(jq -nc --arg k "$KETTLE" --arg t "$TOASTER" --arg b "$BLENDER" '{($k): "Kettle", ($t): "Toaster", ($b): "Blender"}')
    ok "Kettle R200, Toaster R450, Blender R900 (created in that order)"
}

visitor_cart() {
    step "1. A visitor adds items to a cart"
    VISITOR=jar:$TMP/visitor.jar
    call "$SHOP_HOST" "$VISITOR" GET /me
    expect 200 "visitor"
    VISITOR_ID=$(jq -r .user_id <<<"$BODY")
    added() {
        call "$SHOP_HOST" "$1" POST "/carts/$2/items" "{\"sku_id\":\"$3\",\"quantity\":$4}"
        [ "$STATUS" = 200 ]
    }
    poll 15 added "$VISITOR" shopping "$KETTLE" 2 || { echo "$BODY"; fail "add Kettle (is the SKU known to yc-pricing yet?)"; }
    added "$VISITOR" shopping "$KETTLE" 1 || fail "add Kettle again: $STATUS"
    added "$VISITOR" shopping "$TOASTER" 1 || fail "add Toaster: $STATUS"
    added "$VISITOR" wishlist "$BLENDER" 1 || fail "add Blender to the wishlist: $STATUS"
    call "$SHOP_HOST" "$VISITOR" GET /carts/shopping
    expect 200 "visitor cart"
    show_cart
    [ "$(qty "$KETTLE")" = 3 ] || fail "adding again should sum onto the Kettle line"
    ok "visitor $VISITOR_ID: shopping cart priced by yc-pricing via the gateway's internal listener (ADR-021)"
    call "$SHOP_HOST" "$VISITOR" GET /carts
    jq -r '.items[] | "    \(.cart_type): \(.line_count) line(s), \(.quantity) item(s)"' <<<"$BODY"
    ok "multi-cart: shopping and wishlist"
}

register() {
    step "2. The visitor registers; the cart comes along"
    EMAIL="shopper+$RUN@demo-shop.test"
    call "$SHOP_HOST" "$VISITOR" POST /register "{\"email\":\"$EMAIL\",\"password\":\"shopper-password-$RUN\"}"
    expect 201 "register"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    REFRESH=$(jq -r .refresh_token <<<"$BODY")
    USER_ID=$(jq -r .user_id <<<"$BODY")
    [ "$USER_ID" = "$VISITOR_ID" ] || fail "registration should keep the visitor's ID"
    call "$SHOP_HOST" "$CUSTOMER" POST /carts/shopping/items "{\"sku_id\":\"$TOASTER\",\"quantity\":1}"
    expect 200 "customer adds a Toaster"
    show_cart
    [ "$(qty "$KETTLE")" = 3 ] && [ "$(qty "$TOASTER")" = 2 ] || fail "the visitor's cart is not the customer's"
    ok "customer $USER_ID kept the visitor's ID, so the cart is already theirs"

    call "$SHOP_HOST" - POST /refresh "{\"refresh_token\":\"$REFRESH\"}"
    expect 200 "refresh"
    local old=$REFRESH
    CUSTOMER=$(jq -r .access_token <<<"$BODY"); REFRESH=$(jq -r .refresh_token <<<"$BODY")
    call "$SHOP_HOST" - POST /refresh "{\"refresh_token\":\"$old\"}"
    expect 401 "replayed refresh token"
    ok "refresh tokens rotate; replaying the old one is refused (and revokes the login)"
    call "$SHOP_HOST" - POST /login "{\"email\":\"$EMAIL\",\"password\":\"shopper-password-$RUN\"}"
    expect 200 "login again"
    CUSTOMER=$(jq -r .access_token <<<"$BODY"); REFRESH=$(jq -r .refresh_token <<<"$BODY")
}

oauth() {
    step "3. The customer logs out, browses as a new visitor, then signs in with Google"
    call "$SHOP_HOST" "$CUSTOMER" POST /logout "{\"refresh_token\":\"$REFRESH\"}"
    expect 204 "logout"
    call "$SHOP_HOST" - POST /refresh "{\"refresh_token\":\"$REFRESH\"}"
    expect 401 "refresh after logout"
    ok "logged out; the refresh token is revoked"

    local jar=jar:$TMP/visitor2.jar
    call "$SHOP_HOST" "$jar" GET /me
    local visitor2; visitor2=$(jq -r .user_id <<<"$BODY")
    call "$SHOP_HOST" "$jar" POST /carts/shopping/items "{\"sku_id\":\"$KETTLE\",\"quantity\":1}"
    expect 200 "new visitor adds a Kettle"
    ok "anonymous again as visitor $visitor2, with a Kettle in a new cart"

    call "$SHOP_HOST" "$jar" POST /oauth/google/start "{\"login_hint\":\"$EMAIL\"}"
    expect 200 "start Google sign-in"
    local url query code state
    url=$(jq -r .authorization_url <<<"$BODY")
    info "provider redirect: ${url:0:90}…"
    query=${url#*\?}
    code=$(tr '&' '\n' <<<"$query" | sed -n 's/^code=//p')
    state=$(tr '&' '\n' <<<"$query" | sed -n 's/^state=//p')
    call "$SHOP_HOST" "$jar" POST /oauth/google/callback "{\"code\":\"$code\",\"state\":\"$state\"}"
    expect 200 "complete Google sign-in"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    [ "$(jq -r .user_id <<<"$BODY")" = "$USER_ID" ] || fail "Google sign-in did not link to $EMAIL's account"
    ok "signed in with Google (PKCE); the verified email linked the existing account"

    merged() {
        call "$SHOP_HOST" "$CUSTOMER" GET /carts/shopping
        [ "$(qty "$KETTLE")" = 4 ]
    }
    poll 15 merged || { show_cart; fail "the visitor cart did not merge"; }
    show_cart
    [ "$(qty "$TOASTER")" = 2 ] || fail "the Toaster line was lost"
    call "$SHOP_HOST" "$CUSTOMER" GET /carts/wishlist
    [ "$(qty "$BLENDER")" = 1 ] || fail "the wishlist was lost"
    ok "cart intact, the new visitor's Kettle merged in (identity.user.logged_in.v1 → cart.merged.v1)"
    call "$SHOP_HOST" "$jar" GET /carts/shopping
    [ "$(jq '.items | length' <<<"$BODY")" = 0 ] || [ "$STATUS" != 200 ] || fail "the visitor cart still has items"
    ok "the merged visitor cart is gone; merging again on redelivery is a no-op"
}

currency() {
    step "4. A currency change re-renders the cart totals"
    call "$SHOP_HOST" "$CUSTOMER" PATCH /preferences/me '{"display_currency":"USD"}'
    expect 200 "set display currency"
    ok "preferences: display currency USD (preference.currency.changed.v1)"
    in_usd() {
        call "$SHOP_HOST" "$CUSTOMER" GET /carts/shopping
        [ "$(jq -r '.display.currency // ""' <<<"$BODY")" = USD ]
    }
    poll 15 in_usd || fail "no USD totals: yc-pricing needs a ZAR→USD rate from api.frankfurter.dev (docker logs yc-pricing)"
    show_cart
    ok "yc-cart learnt the currency from the event and converts on read; ZAR stays authoritative"
    call "$SHOP_HOST" "$CUSTOMER" GET "/carts/shopping?display_currency=ZAR"
    [ "$(jq -r '.display // "none"' <<<"$BODY")" = none ] || fail "an explicit display_currency should win"
    ok "?display_currency=ZAR overrides the preference"
}

price_change() {
    step "5. An admin base-price change shows \"price changed\""
    call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$KETTLE" '{"price":{"amount_minor":25000,"currency":"ZAR"}}'
    expect 200 201 "new Kettle price"
    call "$SHOP_HOST" "$CUSTOMER" GET /carts/shopping
    expect 200 "cart"
    show_cart
    jq -e --arg k "$KETTLE" '.price_changed and (.items[] | select(.sku_id == $k) |
        .price_changed and .unit_price.amount_minor == 20000 and .current_unit_price.amount_minor == 25000)' <<<"$BODY" \
        >/dev/null || fail "the Kettle should show R200 snapshot, R250 now"
    ok "Kettle: added at R200, now R250 — flagged; totals use the current price"
}

search() {
    step "6. Search sorts by popularity"
    # Popularity counts orders (order.placed.v1) since Phase 7; nothing is
    # ordered here, so popular ties and falls back to the newest first.
    local expected="Blender,Toaster,Kettle"
    ranked() {
        call "$SHOP_HOST" "jar:$TMP/browser.jar" GET "/products?q=$RUN"
        [ "$(jq -r '[.items[].name | split(" ")[0]] | join(",")' <<<"$BODY")" = "$expected" ]
    }
    poll 15 ranked || { echo "$BODY" | head -c 600; fail "visitor order is not $expected"; }
    info "visitor (sort $(jq -r .sort <<<"$BODY")): $(jq -r '[.items[].name] | join(", ")' <<<"$BODY")"
    [ "$(jq -r .sort <<<"$BODY")" = popular ] || fail "visitors should get the popular sort"
    ok "visitors get the popular sort (orders, see demo-phase7.sh); with no orders yet, newest first"
    call "$SHOP_HOST" "$ADMIN" GET "/products?q=$RUN"
    info "shop admin (sort $(jq -r .sort <<<"$BODY")): $(jq -r '[.items[].name] | join(", ")' <<<"$BODY")"
    [ "$(jq -r '[.items[].name | split(" ")[0]] | join(",")' <<<"$BODY")" = "Blender,Toaster,Kettle" ] ||
        fail "shop admins should see the newest first"
    ok "shop admins see the newest first"
    call "$SHOP_HOST" "$CUSTOMER" GET "/products?q=$RUN"
    jq -e '.facets.favourites.product_count == 0' <<<"$BODY" >/dev/null || fail "customers should get the favourites facet"
    ok "customers get an (empty) My favourites facet"
    by_price() {
        call "$SHOP_HOST" "$CUSTOMER" GET "/products?q=$RUN&sort=price_desc"
        [ "$(jq -r '[.items[].name | split(" ")[0]] | join(",")' <<<"$BODY")" = "Blender,Toaster,Kettle" ]
    }
    poll 15 by_price || fail "price sort"
    ok "sort=price_desc uses base prices from pricing.base_price.set.v1"
}

limits() {
    step "Gateway: rate limiting and password reset"
    call "$SHOP_HOST" - POST /password/reset-request "{\"email\":\"$EMAIL\"}"
    expect 202 "reset request"
    ok "password reset requested (202; the event carries no token, dev logs the link: docker logs yc-identity)"

    local jar=$TMP/burst.jar codes
    call "$SHOP_HOST" "jar:$jar" GET /me
    codes=$(for _ in $(seq 1 45); do
        curl -s -o /dev/null -w '%{http_code}\n' -b "$jar" -H "Host: $SHOP_HOST" "$GATEWAY/products?page_size=1"
    done | sort | uniq -c | tr -s ' ' | tr '\n' ' ')
    info "45 rapid requests from one visitor: $codes"
    if grep -q 429 <<<"$codes"; then
        ok "visitors are throttled (burst 30) with 429 and Retry-After"
    else
        info "no 429: rate limiting is off (YC_GATEWAY_RATE_LIMIT=false)"
    fi
}

healthy
provision
shop_admin
visitor_cart
register
oauth
currency
price_change
search
limits
printf '\n\033[32m✓ Phase 3 milestone complete.\033[0m\n'
