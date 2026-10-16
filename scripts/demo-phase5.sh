#!/usr/bin/env bash
# Phase 5 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 5 — Yoco Integration").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase5.sh
#
# Two shops with the same gateways, Yoco first (priority 10) and Stripe
# second (20): demo-shop sells in ZAR, demo-usd in USD. ZAR routes to Yoco
# (Variant B: one charge on Yoco's hosted page, settled by webhook); USD is
# not a Yoco currency, so it routes to Stripe (Variant A). A shop's store
# currency is fixed once it has prices, hence two shops.
#
# The fake Yoco (services/yc-payment/cmd/fake-yoco) and fake Stripe run on
# the host; the fake Yoco's "customer pays" control delivers signed
# webhooks to the shop host through the gateway. With YOCO_API_KEY
# (sk_test_…) and YOCO_WEBHOOK_URL (a public HTTPS URL forwarding to
# $GATEWAY/webhooks/yoco with Host: demo-shop.localhost) the demo uses
# Yoco's test environment and asks you to pay in the browser. Needs curl,
# jq, docker and go. Safe to re-run: each run uses fresh emails and
# products.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
USD_HOST=${USD_HOST:-demo-usd.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
FAKE_YOCO_PORT=${FAKE_YOCO_PORT:-12112}
RUN=$(date +%s)
export NATS_URL=${NATS_URL:-nats://localhost:4222}

envfile() { [ ! -f .env ] || sed -n "s/^$1=//p" .env | tail -1; }
STRIPE_API_KEY=${STRIPE_API_KEY:-$(envfile STRIPE_API_KEY)}
YOCO_API_KEY=${YOCO_API_KEY:-$(envfile YOCO_API_KEY)}
YOCO_WEBHOOK_URL=${YOCO_WEBHOOK_URL:-$(envfile YOCO_WEBHOOK_URL)}

TMP=$(mktemp -d)
PIDS=()
cleanup() {
    for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
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

JQ_MONEY='def amt: "\(.currency) \(.amount_minor / 100 | floor).\(.amount_minor % 100 | tostring | if length < 2 then "0" + . else . end)";'

show_session() {
    jq -r "$JQ_MONEY"'
        "    session \(.session_id)  \(.state)" + (if .failure then "  (\(.failure.reason) in \(.failure.state))" else "" end),
        "    total \(.totals.total | amt)" + (if .payment then "  via \(.payment.gateway) (\(.payment.flow)\(if .payment.hosted then ", hosted" else "" end))" else "" end),
        (if .order then "    order \(.order.order_number // .order.order_id)" else empty end),
        (if .payment.status then "    payment \(.payment.status)" else empty end)' <<<"$BODY"
}

states() {
    jq -r '[.history[] | select(.event_type | IN("started", "confirmed", "state_changed")) | .to_state] | join(" → ")' <<<"$BODY"
}

uuid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/'; }
readyz() { curl -fsS "http://localhost:$1/readyz" >/dev/null; }
routed() { # webhooks are POST-only callback routes
    local method=GET
    [[ $1 != /webhooks/* ]] || method=POST
    ! curl -s -X "$method" -H "Host: $SHOP_HOST" "$GATEWAY$1" | grep -q 'Route not found'
}
psql_payment() { docker exec yc-postgres psql -U postgres -d yc_payment -tAc "$1"; }

# recreate <service> [VAR=value...]: recreate a container with the given
# compose variables and wait until it is ready and routed again.
recreate() {
    local svc=$1 port path; shift
    case $svc in
        yc-payment) port=8089 path=/admin/gateway-config ;;
        yc-orders) port=8092 path=/orders/mine ;;
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
    step "Payments: test keys or the fakes"
    local stripe_base yoco_base
    if [ -n "$STRIPE_API_KEY" ]; then
        [[ $STRIPE_API_KEY == sk_test_* ]] || fail "STRIPE_API_KEY must be a test-mode key (sk_test_…)"
        stripe_base=https://api.stripe.com
        ok "Stripe: api.stripe.com with your test key"
    else
        STRIPE_API_KEY=sk_test_fake_demo
        go build -o "$TMP/fake-stripe" ./services/yc-payment/cmd/fake-stripe
        "$TMP/fake-stripe" -addr ":$FAKE_STRIPE_PORT" >"$TMP/fake-stripe.log" 2>&1 &
        PIDS+=($!)
        poll 10 curl -s "http://localhost:$FAKE_STRIPE_PORT/v1/balance" || fail "fake Stripe did not start"
        stripe_base=http://host.docker.internal:$FAKE_STRIPE_PORT
        ok "Stripe: the fake on localhost:$FAKE_STRIPE_PORT"
    fi
    if [ -n "$YOCO_API_KEY" ]; then
        [[ $YOCO_API_KEY == sk_test_* ]] || fail "YOCO_API_KEY must be a test-mode key (sk_test_…)"
        [ -n "$YOCO_WEBHOOK_URL" ] || fail "YOCO_WEBHOOK_URL is required with YOCO_API_KEY (Yoco needs a public HTTPS URL)"
        FAKE_YOCO=false YOCO_BASE=https://payments.yoco.com yoco_base=https://payments.yoco.com
        HOOK_URL=$YOCO_WEBHOOK_URL
        ok "Yoco: payments.yoco.com with your test key; webhooks to $HOOK_URL"
    else
        FAKE_YOCO=true YOCO_API_KEY=sk_test_fake_yoco_demo YOCO_BASE=http://localhost:$FAKE_YOCO_PORT
        go build -o "$TMP/fake-yoco" ./services/yc-payment/cmd/fake-yoco
        "$TMP/fake-yoco" -addr ":$FAKE_YOCO_PORT" -base "$YOCO_BASE" >"$TMP/fake-yoco.log" 2>&1 &
        PIDS+=($!)
        poll 10 curl -s -H "Authorization: Bearer $YOCO_API_KEY" "$YOCO_BASE/api/webhooks" || fail "fake Yoco did not start"
        yoco_base=http://host.docker.internal:$FAKE_YOCO_PORT
        HOOK_URL="${GATEWAY/localhost/$SHOP_HOST}/webhooks/yoco"
        ok "Yoco: the fake on localhost:$FAKE_YOCO_PORT; it signs webhooks to $HOOK_URL"
    fi
    recreate yc-payment YC_PAYMENT_STRIPE_API_BASE="$stripe_base" YC_PAYMENT_YOCO_API_BASE="$yoco_base"
}

provision() { # provision <tenant> <host> <currency>
    # shellcheck source=lib/provision-tenant.sh
    source scripts/lib/provision-tenant.sh
    provision_tenant "$1" "Demo $3" "$2" \
        --store-currency "$3" --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $2' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $2"
}

# shop <host> <currency> <webhook true|false>: a shop admin, VAT, two
# products and both gateways; sets ADMIN, KETTLE and EBOOK. Yoco delivers
# an account's events to every webhook the account registered, so only the
# shop that takes Yoco payments registers one.
shop() {
    local host=$1 cur=$2 hook=$3 jar=$TMP/owner-$1.jar email="owner+$RUN@$1.test" password="owner-password-$RUN" sys user_id
    docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
        /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
    call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
    expect 200 "system admin login"
    sys=$(jq -r .access_token <<<"$BODY")
    call "$host" "jar:$jar" GET /me
    call "$host" "jar:$jar" POST /register "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 201 "register owner"
    user_id=$(jq -r .user_id <<<"$BODY")
    call "$ADMIN_HOST" "$sys" POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote"
    call "$host" - POST /login "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 200 "shop admin login"
    ADMIN=$(jq -r .access_token <<<"$BODY")

    call "$host" "$ADMIN" PUT /admin/pricing-config "{\"store_currency\":\"$cur\",\"tax_inclusive\":true,\"default_jurisdiction\":\"ZA\"}"
    expect 200 "pricing config"
    call "$host" "$ADMIN" PUT /admin/tax-classes/za-standard \
        '{"display_name":"Standard VAT","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0.15","base":"line_total","sort_order":0}]}'
    expect 200 201 "tax class"
    call "$host" "$ADMIN" GET /admin/inventory/config
    expect 200 "inventory config"
    if [ "$(jq -r .inventory_enabled <<<"$BODY")" != true ]; then
        call "$host" "$ADMIN" PUT /admin/inventory/config "$(jq -c '{inventory_enabled: true,
            default_reservation_ttl_minutes, min_reservation_ttl_minutes, max_reservation_ttl_minutes}' <<<"$BODY")"
        expect 200 "turn inventory on"
    fi
    product() { # product <name> <price minor> <is_digital> <tracked> → sets SKU
        call "$host" "$ADMIN" POST /admin/products "{\"name\":\"$1 $RUN\",\"description\":\"Phase 5 demo\",\"is_digital\":$3}"
        expect 201 "create $1"
        local id; id=$(jq -r .product_id <<<"$BODY")
        call "$host" "$ADMIN" POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\",\"inventory_tracked\":$4$([ "$3" = true ] || echo ',"weight_grams":1500')}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY")
        call "$host" "$ADMIN" PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"$cur\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000 false true; KETTLE=$SKU
    product E-book 9900 true false;  EBOOK=$SKU
    has_stock() { call "$host" "$ADMIN" GET "/admin/stock/$KETTLE"; [ "$STATUS" = 200 ]; }
    poll 15 has_stock || fail "yc-inventory has no stock row for the Kettle"
    call "$host" "$ADMIN" POST "/admin/stock/$KETTLE/receive" '{"quantity":5,"reason":"Phase 5 demo"}'
    expect 200 "receive Kettles"
    call "$host" "$ADMIN" GET /admin/zones
    expect 200 "shipping zones"
    if ! jq -e '.items[] | select(.name == "South Africa")' <<<"$BODY" >/dev/null; then
        call "$host" "$ADMIN" POST /admin/zones '{"name":"South Africa","match_rules":[{"country_code":"ZA"}]}'
        expect 201 "shipping zone"
        call "$host" "$ADMIN" POST /admin/rates "{\"zone_id\":\"$(jq -r .zone_id <<<"$BODY")\",\"method\":\"standard\",
            \"display_name\":\"Standard\",\"pricing_type\":\"flat\",\"flat_amount_minor\":10000}"
        expect 201 "shipping rate"
    fi

    call "$host" "$ADMIN" PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\",\"priority\":20}"
    expect 200 "Stripe credentials"
    call "$host" "$ADMIN" PUT /admin/gateway-config/yoco "{\"enabled\":true,\"api_key\":\"$YOCO_API_KEY\",\"currencies\":[\"USD\"]}"
    expect 400 "Yoco for USD"
    local secret=""
    if $hook; then
        secret=$(curl -fsS -X POST "$YOCO_BASE/api/webhooks" -H "Authorization: Bearer $YOCO_API_KEY" \
            -H "Content-Type: application/json" --data "{\"name\":\"yc-$host-$RUN\",\"url\":\"$HOOK_URL\"}" | jq -r .secret)
        [[ $secret == whsec_* ]] || fail "Yoco did not register the webhook"
        secret=",\"webhook_secret\":\"$secret\""
    fi
    call "$host" "$ADMIN" PUT /admin/gateway-config/yoco "{\"enabled\":true,\"api_key\":\"$YOCO_API_KEY\",\"priority\":10$secret}"
    expect 200 "Yoco credentials"
    call "$host" "$ADMIN" POST /admin/gateway-config/yoco/test
    expect 200 "test Yoco credentials"
    jq -e .ok <<<"$BODY" >/dev/null || fail "Yoco refused the key: $(jq -r .message <<<"$BODY")"
    call "$host" "$ADMIN" GET /admin/gateway-config
    expect 200 "gateway configs"
    jq -r '.items[] | "    \(.gateway): priority \(.priority), currencies \(if (.currencies | length) == 0 then "all it takes" else (.currencies | join(",")) end), enabled \(.enabled)"' <<<"$BODY"
}

shops() {
    step "Two shops, each with Yoco (priority 10) and Stripe (priority 20)"
    provision demo-shop "$SHOP_HOST" ZAR
    provision demo-usd "$USD_HOST" USD
    for path in /checkout /admin/gateway-config /webhooks/yoco /admin/zones; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    shop "$USD_HOST" USD false
    USD_ADMIN=$ADMIN USD_EBOOK=$EBOOK
    shop "$SHOP_HOST" ZAR true
    ok "Yoco refuses a USD currency list (it takes ZAR only); $SHOP_HOST's Yoco webhook is registered and its secret stored"
}

customer() {
    local jar=$TMP/customer.jar
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"shopper+$RUN@demo-shop.test\",\"password\":\"shopper-password-$RUN\"}"
    expect 201 "register"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
}

# fill_cart <host> <auth> <sku> <qty>: make the shopping cart hold one line.
fill_cart() {
    local host=$1 auth=$2 want=$3 qty=$4 sku
    call "$host" "$auth" GET /carts/shopping
    for sku in $(jq -r '.items[]?.sku_id' <<<"$BODY"); do
        call "$host" "$auth" DELETE "/carts/shopping/items/$sku"
    done
    added() { call "$host" "$auth" POST /carts/shopping/items "{\"sku_id\":\"$want\",\"quantity\":$qty}"; [ "$STATUS" = 200 ]; }
    poll 15 added || { echo "$BODY"; fail "add to cart"; }
}

ADDRESS='{"country_code":"ZA","lines":["1 Long Street","Cape Town 8001"],"fields":{"street":"1 Long Street","city":"Cape Town","province":"Western Cape","postal_code":"8001"}}'

start() { # start <host> <auth>; sets SESSION, and ships physical items to ADDRESS
    call "$1" "$2" POST /checkout '{}'
    expect 201 "start checkout"
    SESSION=$(jq -r .session_id <<<"$BODY")
    if [ "$(jq -r .requires_shipping <<<"$BODY")" = true ]; then
        call "$1" "$2" POST "/checkout/$SESSION/shipping" "{\"delivery_address\":$ADDRESS}"
        expect 200 "choose shipping"
    fi
}

# confirm_hosted <key> [curl args...]: confirm in the ZAR shop with no token.
confirm_hosted() {
    local key=$1; shift
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/confirm" \
        "{\"delivery_address\":$ADDRESS,\"return_url\":\"https://$SHOP_HOST/checkout/return\"}" -H "Idempotency-Key: $key" "$@"
}

session_as_admin() { call "$SHOP_HOST" "$ADMIN" GET "/admin/sessions/$SESSION"; }
in_state() { session_as_admin; [ "$(jq -r .state <<<"$BODY")" = "$1" ]; }

# transaction: the session's payment; sets REF (the Yoco checkout) and TX.
transaction() {
    call "$SHOP_HOST" "$ADMIN" GET "/admin/transactions?session_id=$SESSION"
    expect 200 "transactions"
    REF=$(jq -r '.items[0].gateway_reference // empty' <<<"$BODY")
    TX=$(jq -r '.items[0].transaction_id // empty' <<<"$BODY")
}

# customer_pays <success>: the customer finishes on Yoco's page.
customer_pays() {
    if $FAKE_YOCO; then
        curl -fsS -X POST "$YOCO_BASE/_fake/checkouts/$REF/pay?success=$1" -o "$TMP/paid"
        EVENT=$(jq -r .event_id "$TMP/paid")
        info "Yoco webhook $EVENT → $(jq -r '[.deliveries[] | "\(.url) \(.status)"] | join(", ")' "$TMP/paid")"
        jq -e '[.deliveries[].status] | all(. == 200)' "$TMP/paid" >/dev/null || fail "the webhook was not accepted"
    else
        info "pay on Yoco's test page (card 4111 1111 1111 1111, any future date and CVV): $PAY_URL"
    fi
}

yoco_happy() {
    customer
    fill_cart "$SHOP_HOST" "$CUSTOMER" "$KETTLE" 1

    step "1. A ZAR checkout routes to Yoco"
    start "$SHOP_HOST" "$CUSTOMER"
    show_session
    jq -e '.payment.gateway == "yoco" and .payment.flow == "charge" and .payment.hosted' <<<"$BODY" >/dev/null ||
        fail "want Yoco, Variant B, hosted"
    ok "Yoco is first for ZAR; it cannot authorize separately, so the session will charge once (Variant B)"

    step "2. Confirm: order first, then CHARGING on Yoco's page"
    KEY=$(uuid)
    confirm_hosted "$KEY"
    expect 202 "confirm"
    show_session
    PAY_URL=$(jq -r '.payment.next_action.redirect_to_url.url' <<<"$BODY")
    jq -e '.state == "CHARGING" and .payment.status == "pending" and .order.order_id' <<<"$BODY" >/dev/null ||
        fail "want CHARGING, pending, with a pending order"
    info "redirect the customer to $PAY_URL"
    transaction
    ok "202: order created (pending_payment) before any money moves; Yoco checkout $REF awaits the customer"

    step "3. The customer pays; Yoco's webhook completes the checkout"
    customer_pays true
    poll 300 in_state COMPLETED || { show_session; fail "the webhook did not complete the session"; }
    info "$(states)"
    [ "$(states)" = "PRICING → RESERVING → ORDER_CREATE → CHARGING → CONFIRMING → COMPLETED" ] || fail "unexpected state path"
    call "$SHOP_HOST" "$CUSTOMER" GET "/checkout/$SESSION"
    show_session
    local number; number=$(jq -r .order.order_number <<<"$BODY")
    transaction
    jq -e '.items[0].status == "captured" and .items[0].kind == "charge"' <<<"$BODY" >/dev/null || fail "want a captured charge"
    ok "Yoco's payment.succeeded webhook → payment.charged.v1 → checkout placed order $number"

    if $FAKE_YOCO; then
        step "4. A replayed webhook changes nothing"
        curl -fsS -X POST "$YOCO_BASE/_fake/events/$EVENT/redeliver" -o "$TMP/again"
        jq -e '[.deliveries[].status] | all(. == 200)' "$TMP/again" >/dev/null || fail "the replay was refused"
        local stored charged
        stored=$(psql_payment "SELECT count(*) FROM webhook_events WHERE gateway = 'yoco' AND gateway_event_id = '$EVENT'")
        charged=$(psql_payment "SELECT count(*) FROM outbox WHERE envelope->>'event_type' = 'payment.charged.v1'
            AND envelope->'payload'->>'transaction_id' = '$TX'")
        info "webhook_events rows for $EVENT: $stored; payment.charged.v1 for the transaction: $charged"
        [ "$stored" = 1 ] && [ "$charged" = 1 ] || fail "the replay was applied twice"
        ok "Yoco redelivered the event (200); it is recorded once and charged once"
    fi
}

order_failure() {
    step "5. Order creation fails: no money moves"
    fill_cart "$SHOP_HOST" "$CUSTOMER" "$KETTLE" 1
    start "$SHOP_HOST" "$CUSTOMER"
    docker stop yc-orders >/dev/null
    info "yc-orders stopped"
    confirm_hosted "$(uuid)" --max-time 60 || true
    info "confirm answered ${STATUS:-nothing} in $(jq -r '.state // "-"' <<<"$BODY" 2>/dev/null)"
    recreate yc-orders
    poll 120 in_state FAILED || { show_session; fail "compensation did not finish"; }
    jq -e '.failure.state == "ORDER_CREATE"' <<<"$BODY" >/dev/null || fail "want a failure in ORDER_CREATE"
    info "$(states)"
    jq -r '.compensation_log[] | "    undo: \(.step) (\(.result))"' <<<"$BODY"
    transaction
    [ -z "$TX" ] || fail "a payment was attempted"
    ok "the order comes before the charge, so the failure needed no refund (ADR-019 rule 3)"
}

late_payment() {
    step "6. Cancelled while on Yoco's page; the customer pays anyway"
    fill_cart "$SHOP_HOST" "$CUSTOMER" "$KETTLE" 1
    start "$SHOP_HOST" "$CUSTOMER"
    confirm_hosted "$(uuid)"
    expect 202 "confirm"
    PAY_URL=$(jq -r '.payment.next_action.redirect_to_url.url' <<<"$BODY")
    transaction
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/cancel"
    expect 200 "cancel"
    show_session
    session_as_admin
    jq -r '.compensation_log[] | "    undo: \(.step) (\(.result))"' <<<"$BODY"
    jq -e '.compensation_log[] | select(.step == "payment_voided" and .result == "left_open")' <<<"$BODY" >/dev/null ||
        fail "want the Yoco checkout left open"
    ok "cancelled: order abandoned, stock released; Yoco cannot close its page, so the checkout is left open"
    if ! $FAKE_YOCO; then
        info "with real Yoco, pay on $PAY_URL now to see the refund (waiting up to 5 minutes)"
    fi
    customer_pays true
    refunded() { session_as_admin; jq -e '.compensation_log[] | select(.step == "late_charge_refunded")' <<<"$BODY"; }
    poll 300 refunded || fail "the late payment was not refunded"
    jq -r '.compensation_log[] | "    undo: \(.step) (\(.result))"' <<<"$BODY"
    transaction
    jq -e '.items[0].status == "refunded"' <<<"$BODY" >/dev/null || fail "want the transaction refunded"
    ok "payment.charged.v1 for an ended session: checkout refunded it (Variant B compensation)"
}

usd() {
    step "7. A USD checkout routes to Stripe (Variant A)"
    local visitor=jar:$TMP/usd.jar
    call "$USD_HOST" "$visitor" GET /me
    fill_cart "$USD_HOST" "$visitor" "$USD_EBOOK" 1
    start "$USD_HOST" "$visitor"
    show_session
    jq -e '.payment.gateway == "stripe" and .payment.flow == "auth_capture" and (.payment.hosted | not)' <<<"$BODY" >/dev/null ||
        fail "want Stripe, Variant A"
    call "$USD_HOST" "$visitor" POST "/checkout/$SESSION/confirm" \
        "{\"payment_gateway_token\":\"pm_card_visa\",\"guest_contact\":{\"email\":\"guest+$RUN@demo-usd.test\",\"phone_e164\":\"+27831234567\"}}" \
        -H "Idempotency-Key: $(uuid)"
    expect 200 "USD confirm"
    show_session
    call "$USD_HOST" "$USD_ADMIN" GET "/admin/sessions/$SESSION"
    info "$(states)"
    [ "$(states)" = "PRICING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED" ] || fail "unexpected state path"
    ok "Yoco is first but takes no USD, so Stripe was routed; it authorizes separately, so Variant A ran"
}

switch() {
    step "8. The customer picks Stripe instead at confirm"
    fill_cart "$SHOP_HOST" "$CUSTOMER" "$EBOOK" 1
    start "$SHOP_HOST" "$CUSTOMER"
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/confirm" \
        '{"payment_gateway":"stripe","payment_gateway_token":"pm_card_visa"}' -H "Idempotency-Key: $(uuid)"
    expect 200 "confirm with Stripe"
    show_session
    session_as_admin
    info "$(states)"
    ok "same ZAR shop, Stripe chosen: the gateway's capabilities picked Variant A"
}

healthy
payments
shops
yoco_happy
order_failure
late_payment
usd
switch
printf '\n\033[32m✓ Phase 5 milestone complete.\033[0m\n'
