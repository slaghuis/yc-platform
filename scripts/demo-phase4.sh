#!/usr/bin/env bash
# Phase 4 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 4 — Checkout Happy Path").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase4.sh
#
# Payments use Stripe test mode when STRIPE_API_KEY (sk_test_…) is set in
# the environment or .env; otherwise the fake Stripe in
# services/yc-payment/cmd/fake-stripe, run on the host. Either way the
# script points yc-payment at it (recreating the container). The resumption
# step recreates yc-checkout with a dev-only pause, kills it mid-saga and
# starts it again. Needs curl, jq, docker and go. Safe to re-run: each run
# uses fresh emails and products.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
TENANT=${TENANT:-demo-shop}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
JAEGER=${JAEGER_URL:-http://localhost:16686}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
RUN=$(date +%s)
export NATS_URL=${NATS_URL:-nats://localhost:4222}

if [ -f .env ]; then
    [ -n "${STRIPE_API_KEY:-}" ] || STRIPE_API_KEY=$(sed -n 's/^STRIPE_API_KEY=//p' .env | tail -1)
    [ -n "${STRIPE_WEBHOOK_SECRET:-}" ] || STRIPE_WEBHOOK_SECRET=$(sed -n 's/^STRIPE_WEBHOOK_SECRET=//p' .env | tail -1)
fi
STRIPE_API_KEY=${STRIPE_API_KEY:-}
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET:-}

TMP=$(mktemp -d)
FAKE_PID=
cleanup() {
    [ -z "$FAKE_PID" ] || kill "$FAKE_PID" 2>/dev/null || true
    rm -rf "$TMP"
}
trap cleanup EXIT

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

# show_session prints the checkout session in BODY.
show_session() {
    jq -r "$JQ_MONEY"'
        "    session \(.session_id)  \(.state)" + (if .failure then "  (\(.failure.reason))" else "" end),
        (.items[] | "    \(.name) × \(.quantity)  \(.line_total | amt)" +
            (if .is_digital then "  digital" else "" end) + (if .inventory_tracked then "  tracked" else "" end)),
        "    items \(.totals.items_total | amt)  incl. VAT \(.totals.tax_total | amt)  shipping \(.totals.shipping | amt)  total \(.totals.total | amt)",
        (if .order then "    order \(.order.order_number)" else empty end),
        (if .payment.status then "    payment \(.payment.status)" else empty end)' <<<"$BODY"
}

# states prints the saga's state changes from an admin session in BODY.
states() {
    jq -r '[.history[] | select(.event_type | IN("started", "confirmed", "state_changed")) | .to_state] | join(" → ")' <<<"$BODY"
}

# cleared <auth>: the shopping cart is empty (checkout.completed.v1 is
# applied asynchronously, so wait for it before refilling the cart).
cleared() {
    call "$SHOP_HOST" "$1" GET /carts/shopping
    [ "$(jq '.items | length' <<<"$BODY")" = 0 ]
}

uuid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/'; }

readyz() { curl -fsS "http://localhost:$1/readyz" >/dev/null; }
routed() { ! curl -s -H "Host: $SHOP_HOST" "$GATEWAY$1" | grep -q 'Route not found'; }

# recreate <service> [VAR=value...]: recreate a container with the given
# compose variables and wait until it is ready and routed again.
recreate() {
    local svc=$1 port path; shift
    case $svc in
        yc-payment) port=8089 path=/admin/gateway-config ;;
        yc-checkout) port=8093 path=/checkout ;;
    esac
    env "$@" docker compose --profile app up -d --no-build "$svc" 2>&1 | sed 's/^/    /'
    poll 60 readyz "$port" || fail "$svc did not come back (docker logs $svc)"
    poll 45 routed "$path" || fail "no gateway route for $path after restarting $svc"
}

healthy() {
    step "0. Services are up"
    for port in 8090 8081 8083 8084 8087 8088 8089 8092 8093 8094; do
        readyz "$port" || fail "localhost:$port/readyz (task devstack:app)"
    done
    ok "gateway, identity, product, pricing, cart, inventory, payment, orders, checkout and shipping are ready"
}

payments() {
    step "Payments: Stripe test mode or the fake"
    if [ -n "$STRIPE_API_KEY" ]; then
        [[ $STRIPE_API_KEY == sk_test_* ]] || fail "STRIPE_API_KEY must be a test-mode key (sk_test_…)"
        FAKE=false
        recreate yc-payment YC_PAYMENT_STRIPE_API_BASE=https://api.stripe.com
        ok "yc-payment uses api.stripe.com with your test key; payments show in the Stripe dashboard"
    else
        FAKE=true
        STRIPE_API_KEY=sk_test_fake_demo
        go build -o "$TMP/fake-stripe" ./services/yc-payment/cmd/fake-stripe
        "$TMP/fake-stripe" -addr ":$FAKE_STRIPE_PORT" >"$TMP/fake-stripe.log" 2>&1 &
        FAKE_PID=$!
        poll 10 curl -s "http://localhost:$FAKE_STRIPE_PORT/v1/balance" || fail "fake Stripe did not start ($TMP/fake-stripe.log)"
        recreate yc-payment YC_PAYMENT_STRIPE_API_BASE="http://host.docker.internal:$FAKE_STRIPE_PORT"
        ok "no STRIPE_API_KEY: yc-payment uses the fake Stripe on localhost:$FAKE_STRIPE_PORT"
    fi
}

provision() {
    step "Provision tenant $TENANT ($SHOP_HOST, ZAR, VAT-inclusive)"
    go run ./tools/yc-tenant-cli provision --tenant-id "$TENANT" --name "Demo Shop" --hostname "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"
    for path in /products /carts /checkout /orders/mine /admin/stock /admin/gateway-config /admin/zones; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "checkout, orders, inventory and payment routes registered with the gateway"
}

shop_admin() {
    step "Shop admin: catalog, stock and Stripe credentials"
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
    call "$SHOP_HOST" "$ADMIN" GET /admin/inventory/config
    expect 200 "inventory config"
    if [ "$(jq -r .inventory_enabled <<<"$BODY")" != true ]; then
        call "$SHOP_HOST" "$ADMIN" PUT /admin/inventory/config "$(jq -c '{inventory_enabled: true,
            default_reservation_ttl_minutes, min_reservation_ttl_minutes, max_reservation_ttl_minutes}' <<<"$BODY")"
        expect 200 "turn inventory on"
    fi
    info "reservations hold stock for $(jq -r .default_reservation_ttl_minutes <<<"$BODY") minutes"

    product() { # product <name> <price minor> <is_digital> <tracked> → sets SKU
        call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"$1 $RUN\",\"description\":\"Phase 4 demo\",\"is_digital\":$3}"
        expect 201 "create $1"
        local id; id=$(jq -r .product_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\",\"inventory_tracked\":$4$([ "$3" = true ] || echo ',"weight_grams":1500')}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000 false true;  KETTLE=$SKU
    product E-book 9900 true false;   EBOOK=$SKU
    ok "Kettle R200 (physical, stock tracked) and E-book R99 (digital, untracked)"

    has_stock() { call "$SHOP_HOST" "$ADMIN" GET "/admin/stock/$KETTLE"; [ "$STATUS" = 200 ]; }
    poll 15 has_stock || fail "yc-inventory has no stock row for the Kettle (product.sku.created.v1)"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/stock/$KETTLE/receive" '{"quantity":5,"reason":"Phase 4 demo"}'
    expect 200 "receive Kettles"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/stock/$EBOOK"
    expect 404 "the E-book is untracked and has no stock"
    ok "received 5 Kettles; the E-book has no stock record (unlimited)"

    call "$SHOP_HOST" "$ADMIN" GET /admin/zones
    expect 200 "shipping zones"
    if ! jq -e '.items[] | select(.name == "South Africa")' <<<"$BODY" >/dev/null; then
        call "$SHOP_HOST" "$ADMIN" POST /admin/zones '{"name":"South Africa","match_rules":[{"country_code":"ZA"}]}'
        expect 201 "shipping zone"
        call "$SHOP_HOST" "$ADMIN" POST /admin/rates "{\"zone_id\":\"$(jq -r .zone_id <<<"$BODY")\",\"method\":\"standard\",
            \"display_name\":\"Standard\",\"pricing_type\":\"flat\",\"flat_amount_minor\":10000}"
        expect 201 "shipping rate"
    fi
    ok "ships anywhere in South Africa: Standard R100 (yc-shipping)"

    local secret=""
    [ -z "${STRIPE_WEBHOOK_SECRET:-}" ] || secret=",\"webhook_secret\":\"$STRIPE_WEBHOOK_SECRET\""
    call "$SHOP_HOST" "$ADMIN" PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\"$secret}"
    expect 200 "Stripe credentials"
    info "stored encrypted; shown as $(jq -r .api_key_hint <<<"$BODY")"
    call "$SHOP_HOST" "$ADMIN" POST /admin/gateway-config/stripe/test
    expect 200 "test Stripe credentials"
    jq -e .ok <<<"$BODY" >/dev/null || fail "Stripe refused the key: $(jq -r .message <<<"$BODY")"
    ok "Stripe accepts the shop's key"
}

customer() {
    step "A customer fills a cart"
    local jar=$TMP/customer.jar
    EMAIL="shopper+$RUN@demo-shop.test"
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$EMAIL\",\"password\":\"shopper-password-$RUN\"}"
    expect 201 "register"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    added() {
        call "$SHOP_HOST" "$CUSTOMER" POST /carts/shopping/items "{\"sku_id\":\"$1\",\"quantity\":$2}"
        [ "$STATUS" = 200 ]
    }
    poll 15 added "$KETTLE" 2 || { echo "$BODY"; fail "add Kettles (is the SKU known to yc-pricing yet?)"; }
    added "$EBOOK" 1 || fail "add the E-book: $STATUS"
    ok "$EMAIL: 2 × Kettle (tracked) and 1 × E-book (digital, untracked)"
}

# fill_cart <auth> <sku> <qty> [<sku> <qty>...]: make the shopping cart
# hold exactly these lines.
fill_cart() {
    local auth=$1 sku; shift
    call "$SHOP_HOST" "$auth" GET /carts/shopping
    for sku in $(jq -r '.items[]?.sku_id' <<<"$BODY"); do
        call "$SHOP_HOST" "$auth" DELETE "/carts/shopping/items/$sku"
    done
    while [ $# -gt 0 ]; do
        call "$SHOP_HOST" "$auth" POST /carts/shopping/items "{\"sku_id\":\"$1\",\"quantity\":$2}"
        expect 200 "add to cart"
        shift 2
    done
}

ADDRESS='{"country_code":"ZA","lines":["1 Long Street","Cape Town 8001"],"fields":{"street":"1 Long Street","city":"Cape Town","province":"Western Cape","postal_code":"8001"}}'

# start <auth>: start checkout from the shopping cart and, for physical
# items, choose shipping to ADDRESS; sets SESSION.
start() {
    call "$SHOP_HOST" "$1" POST /checkout '{}'
    expect 201 "start checkout"
    SESSION=$(jq -r .session_id <<<"$BODY")
    if [ "$(jq -r .requires_shipping <<<"$BODY")" = true ]; then
        call "$SHOP_HOST" "$1" POST "/checkout/$SESSION/shipping" "{\"delivery_address\":$ADDRESS}"
        expect 200 "choose shipping"
    fi
}

# confirm <auth> <key> <payment method> [extra json fields] [curl args...]
confirm() {
    local auth=$1 key=$2 pm=$3 extra=${4:-}
    shift 4 2>/dev/null || shift $#
    call "$SHOP_HOST" "$auth" POST "/checkout/$SESSION/confirm" \
        "{\"payment_gateway_token\":\"$pm\",\"return_url\":\"https://$SHOP_HOST/checkout/return\"$extra}" \
        -H "Idempotency-Key: $key" "$@"
}

stock() { # stock <sku>: prints on_hand/reserved
    call "$SHOP_HOST" "$ADMIN" GET "/admin/stock/$1"
    jq -r '"\(.on_hand)/\(.reserved)"' <<<"$BODY"
}

happy() {
    step "1. Checkout: quote"
    start "$CUSTOMER"
    show_session
    jq -e '.state == "PRICING" and .requires_shipping and (.items | length) == 2' <<<"$BODY" >/dev/null ||
        fail "want a PRICING quote that needs shipping"
    info "shipping: $(jq -r "$JQ_MONEY"'[.shipping_options[] | "\(.name) \(.amount | amt)"] | join(", ")' <<<"$BODY")"
    ok "PRICING: priced by yc-pricing; the Kettle is physical, so shipping to the address was quoted by yc-shipping"

    step "2. Checkout: confirm with pm_card_visa"
    KEY=$(uuid)
    local trace span
    trace=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); span=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
    confirm "$CUSTOMER" "$KEY" pm_card_visa ",\"delivery_address\":$ADDRESS" -H "traceparent: 00-$trace-$span-01"
    expect 200 "confirm"
    show_session
    [ "$(jq -r .state <<<"$BODY")" = COMPLETED ] || fail "want COMPLETED"
    ORDER_ID=$(jq -r .order.order_id <<<"$BODY")
    ORDER_NUMBER=$(jq -r .order.order_number <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"
    expect 200 "admin session"
    local path; path=$(states)
    info "$path"
    [ "$path" = "PRICING → RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED" ] ||
        fail "unexpected state path"
    ok "the saga ran every step synchronously (ADR-019) and completed"

    confirm "$CUSTOMER" "$KEY" pm_card_visa ",\"delivery_address\":$ADDRESS"
    expect 200 "replay confirm"
    [ "$(jq -r .order.order_id <<<"$BODY")" = "$ORDER_ID" ] || fail "the replay made a second order"
    confirm "$CUSTOMER" "$(uuid)" pm_card_visa ",\"delivery_address\":$ADDRESS"
    expect 409 "confirm with another key"
    ok "same Idempotency-Key replays the result; another key is refused (409)"

    step "3. The order, the payment, the cart and the stock"
    call "$SHOP_HOST" "$CUSTOMER" GET /orders/mine
    expect 200 "my orders"
    jq -r "$JQ_MONEY"'.items[] | "    \(.order_number)  \(.status)  \(.item_count) item(s)  \(.total | amt)"' <<<"$BODY"
    jq -e --arg id "$ORDER_ID" '.items[] | select(.order_id == $id and .status == "received")' <<<"$BODY" >/dev/null ||
        fail "order $ORDER_NUMBER is not in /orders/mine as received"
    ok "order $ORDER_NUMBER is in /orders/mine"

    call "$SHOP_HOST" "$ADMIN" GET "/admin/transactions?session_id=$SESSION"
    expect 200 "transactions"
    local pi; pi=$(jq -r '.items[0].gateway_reference' <<<"$BODY")
    jq -e '.items[0].status == "captured"' <<<"$BODY" >/dev/null || fail "the payment is not captured"
    if $FAKE; then
        ok "payment captured ($pi on the fake Stripe)"
    else
        ok "payment captured: https://dashboard.stripe.com/test/payments/$pi"
    fi

    poll 15 cleared "$CUSTOMER" || fail "the cart still has items (checkout.completed.v1 → yc-cart)"
    ok "cart cleared (checkout.completed.v1)"

    [ "$(stock "$KETTLE")" = 3/0 ] || fail "Kettle stock is $(stock "$KETTLE") on hand/reserved, want 3/0"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/stock/$EBOOK"
    expect 404 "E-book stock"
    ok "Kettle stock 5 → 3 (reservation committed); the untracked E-book has no stock to decrement"

    traced() {
        curl -fsS "$JAEGER/api/traces/$trace" -o "$TMP/trace" &&
            jq -e '[.data[0].processes[].serviceName] | contains(["yc-gateway","yc-checkout","yc-inventory","yc-payment","yc-orders"])' \
                "$TMP/trace"
    }
    if poll 20 traced; then
        info "services: $(jq -r '[.data[0].processes[].serviceName] | unique | join(", ")' "$TMP/trace")"
        ok "one Jaeger trace covers the checkout: $JAEGER/trace/$trace"
    else
        info "trace $trace not found in Jaeger yet; search $JAEGER/search?service=yc-checkout"
    fi
}

declined() {
    step "4. A declined card compensates"
    fill_cart "$CUSTOMER" "$KETTLE" 1
    start "$CUSTOMER"
    confirm "$CUSTOMER" "$(uuid)" pm_card_chargeDeclined ",\"delivery_address\":$ADDRESS"
    expect 422 "declined confirm"
    show_session
    jq -e '.state == "FAILED" and .failure.reason == "payment_declined"' <<<"$BODY" >/dev/null || fail "want FAILED payment_declined"
    info "decline reason: $(jq -r '.failure.detail.decline_reason // "-"' <<<"$BODY")"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"
    info "$(states)"
    jq -r '.compensation_log[] | "    undo: \(.step) (\(.result))"' <<<"$BODY"
    jq -e '[.compensation_log[].step] | index("reservation_released")' <<<"$BODY" >/dev/null ||
        fail "the reservation was not released"
    [ "$(stock "$KETTLE")" = 3/0 ] || fail "Kettle stock is $(stock "$KETTLE"), want 3/0"
    ok "authorisation failed, so no order was created; the reservation was released and stock is back to 3/0"
    call "$SHOP_HOST" "$CUSTOMER" GET /carts/shopping
    [ "$(jq '.items | length' <<<"$BODY")" = 1 ] || fail "a failed checkout should leave the cart alone"
    ok "the cart is untouched, ready to try another card"
}

out_of_stock() {
    step "5. Out of stock fails fast, before any payment"
    fill_cart "$CUSTOMER" "$KETTLE" 10
    start "$CUSTOMER"
    confirm "$CUSTOMER" "$(uuid)" pm_card_visa ",\"delivery_address\":$ADDRESS"
    expect 422 "out-of-stock confirm"
    show_session
    jq -e '.state == "FAILED" and .failure.reason == "insufficient_stock"' <<<"$BODY" >/dev/null ||
        fail "want FAILED insufficient_stock"
    jq -r '.failure.detail.shortages[]? | "    short: \(tojson)"' <<<"$BODY"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/transactions?session_id=$SESSION"
    expect 200 "transactions"
    [ "$(jq '.items | length' <<<"$BODY")" = 0 ] || fail "a payment was attempted"
    ok "RESERVING refused 10 Kettles (3 left); the card was never charged"
}

guest() {
    step "6. Guest checkout of a digital item"
    local visitor=jar:$TMP/guest.jar
    call "$SHOP_HOST" "$visitor" GET /me
    expect 200 "visitor"
    call "$SHOP_HOST" "$visitor" POST /carts/shopping/items "{\"sku_id\":\"$EBOOK\",\"quantity\":1}"
    expect 200 "visitor adds the E-book"
    start "$visitor"
    jq -e '.requires_shipping == false and .totals.shipping.amount_minor == 0' <<<"$BODY" >/dev/null ||
        fail "digital-only should need no shipping"
    confirm "$visitor" "$(uuid)" pm_card_visa \
        ",\"guest_contact\":{\"email\":\"guest+$RUN@demo-shop.test\",\"phone_e164\":\"+27831234567\"}"
    expect 200 "guest confirm"
    show_session
    jq -e '.state == "COMPLETED" and .user_type == "visitor"' <<<"$BODY" >/dev/null || fail "want a COMPLETED visitor checkout"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"
    info "$(states)"
    ok "a visitor paid with contact details only: no address, no shipping, no RESERVING (nothing tracked)"
}

three_ds() {
    step "7. 3-D Secure: the payment waits on the customer"
    fill_cart "$CUSTOMER" "$KETTLE" 1
    start "$CUSTOMER"
    local key; key=$(uuid)
    confirm "$CUSTOMER" "$key" pm_card_threeDSecure2Required ",\"delivery_address\":$ADDRESS"
    expect 202 "3DS confirm"
    show_session
    info "next_action: $(jq -c '.payment.next_action' <<<"$BODY" | head -c 120)"
    jq -e '.state == "AUTHORIZING" and .payment.status == "pending"' <<<"$BODY" >/dev/null || fail "want AUTHORIZING, pending"
    ok "202: the session waits in AUTHORIZING while the customer completes the challenge"
    if $FAKE; then
        call "$SHOP_HOST" "$ADMIN" GET "/admin/transactions?session_id=$SESSION"
        local pi; pi=$(jq -r '.items[0].gateway_reference' <<<"$BODY")
        curl -fsS -X POST "http://localhost:$FAKE_STRIPE_PORT/_fake/payment_intents/$pi/complete_action?success=true"
        confirm "$CUSTOMER" "$key" pm_card_threeDSecure2Required ",\"delivery_address\":$ADDRESS"
        expect 200 "confirm again after 3DS"
        [ "$(jq -r .state <<<"$BODY")" = COMPLETED ] || fail "want COMPLETED after 3DS"
        poll 15 cleared "$CUSTOMER" || fail "the cart was not cleared"
        ok "challenge passed; confirming again with the same key finished the checkout"
    else
        info "with real Stripe the customer follows next_action in the browser; the demo cancels instead"
        call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/cancel"
        expect 200 "cancel"
        show_session
        ok "cancelled: the authorisation was voided and the reservation released"
    fi
}

resume() {
    step "8. Resumption: kill yc-checkout mid-saga"
    fill_cart "$CUSTOMER" "$KETTLE" 1
    info "recreating yc-checkout to pause 30s before CAPTURING (dev only)"
    recreate yc-checkout YC_CHECKOUT_PAUSE_BEFORE=CAPTURING YC_CHECKOUT_PAUSE_FOR=30s
    start "$CUSTOMER"
    local before; before=$(stock "$KETTLE")
    confirm "$CUSTOMER" "$(uuid)" pm_card_visa ",\"delivery_address\":$ADDRESS" --max-time 3 2>/dev/null || true
    paused() {
        call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"
        [ "$(jq -r .state <<<"$BODY")" = CAPTURING ]
    }
    poll 20 paused || { show_session; fail "the saga did not reach CAPTURING"; }
    info "$(states)  (paused, lease held)"
    docker kill yc-checkout >/dev/null
    ok "killed yc-checkout with the payment authorised and order $(jq -r .order.order_id <<<"$BODY") awaiting payment"
    recreate yc-checkout YC_CHECKOUT_PAUSE_BEFORE= YC_CHECKOUT_PAUSE_FOR=0s
    done_() {
        call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"
        [ "$(jq -r .state <<<"$BODY")" = COMPLETED ]
    }
    poll 60 done_ || { show_session; fail "the restarted yc-checkout did not finish the saga"; }
    info "$(states)"
    ok "on restart it released its old lease and the sweeper finished the saga from CAPTURING (stock $before → $(stock "$KETTLE"))"
}

expiry() {
    step "9. Expiry"
    fill_cart "$CUSTOMER" "$EBOOK" 1
    start "$CUSTOMER"
    info "session created $(jq -r .created_at <<<"$BODY"), expires $(jq -r .expires_at <<<"$BODY")"
    ok "sessions expire after 30 minutes and reservations after 15; the sweeper compensates (see the integration tests)"
    if ! $FAKE; then
        info "Stripe webhooks: stripe listen --forward-to $GATEWAY/webhooks/stripe -H 'Host: $SHOP_HOST'"
        info "then set the printed whsec_ as STRIPE_WEBHOOK_SECRET and re-run"
    fi
}

healthy
payments
provision
shop_admin
customer
happy
declined
out_of_stock
guest
three_ds
resume
expiry
printf '\n\033[32m✓ Phase 4 milestone complete.\033[0m\n'
