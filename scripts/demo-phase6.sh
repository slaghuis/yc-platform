#!/usr/bin/env bash
# Phase 6 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 6 — Order Fulfillment").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase6.sh
#
# The shop ships to three zones, matched in priority order: Gauteng (flat
# rates), Western Cape (weight tiers) and the rest of South Africa (price
# tiers, free from R1000). A customer's options follow the address; the
# order then moves received → packaged → shipped → delivered as the shop
# admin books the shipment (yc-orders → yc-shipping) and updates it
# (shipping.* events → yc-orders); a return is accepted and refunded
# through yc-payment. An all-digital order needs no address and is never
# shipped.
#
# Payments use Stripe test mode when STRIPE_API_KEY (sk_test_…) is set in
# the environment or .env; otherwise the fake Stripe on the host. Needs
# curl, jq, docker and go. Safe to re-run: the zones are replaced, and each
# run uses fresh emails and products.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
TENANT=${TENANT:-demo-shop}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
RUN=$(date +%s)
export NATS_URL=${NATS_URL:-nats://localhost:4222}

if [ -f .env ] && [ -z "${STRIPE_API_KEY:-}" ]; then
    STRIPE_API_KEY=$(sed -n 's/^STRIPE_API_KEY=//p' .env | tail -1)
fi
STRIPE_API_KEY=${STRIPE_API_KEY:-}

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

JQ_MONEY='def amt: "\(.currency) \(.amount_minor / 100 | floor).\(.amount_minor % 100 | tostring | if length < 2 then "0" + . else . end)";'
rand() { printf 'R%d.%02d' $(($1 / 100)) $(($1 % 100)); }

# options prints the session's shipping options in BODY, * on the selected.
options() {
    jq -r "$JQ_MONEY"'.selected_shipping_option_id as $sel |
        (.shipping_options[] | "    \(if .id == $sel then "*" else " " end) \(.name) (\(.id))  \(.amount | amt)" +
            (if .estimated_days_min == null then ""
             elif .estimated_days_min == .estimated_days_max then "  \(.estimated_days_min) day(s)"
             else "  \(.estimated_days_min)-\(.estimated_days_max) days" end)),
        "    items \(.totals.items_total | amt)  shipping \(.totals.shipping | amt)  total \(.totals.total | amt)"' <<<"$BODY"
}

uuid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/'; }
readyz() { curl -fsS "http://localhost:$1/readyz" >/dev/null; }
routed() { ! curl -s -H "Host: $SHOP_HOST" "$GATEWAY$1" | grep -q 'Route not found'; }

healthy() {
    step "0. Services are up"
    for port in 8090 8081 8083 8084 8087 8088 8089 8092 8093 8094; do
        readyz "$port" || fail "localhost:$port/readyz (task devstack:app)"
    done
    ok "gateway, identity, product, pricing, cart, inventory, payment, orders, checkout and shipping are ready"
}

payments() {
    step "Payments: Stripe test mode or the fake"
    local base
    if [ -n "$STRIPE_API_KEY" ]; then
        [[ $STRIPE_API_KEY == sk_test_* ]] || fail "STRIPE_API_KEY must be a test-mode key (sk_test_…)"
        base=https://api.stripe.com
        ok "yc-payment uses api.stripe.com with your test key"
    else
        STRIPE_API_KEY=sk_test_fake_demo
        go build -o "$TMP/fake-stripe" ./services/yc-payment/cmd/fake-stripe
        "$TMP/fake-stripe" -addr ":$FAKE_STRIPE_PORT" >"$TMP/fake-stripe.log" 2>&1 &
        FAKE_PID=$!
        poll 10 curl -s "http://localhost:$FAKE_STRIPE_PORT/v1/balance" || fail "fake Stripe did not start"
        base=http://host.docker.internal:$FAKE_STRIPE_PORT
        ok "no STRIPE_API_KEY: yc-payment uses the fake Stripe on localhost:$FAKE_STRIPE_PORT"
    fi
    YC_PAYMENT_STRIPE_API_BASE=$base docker compose --profile app up -d --no-build yc-payment 2>&1 | sed 's/^/    /'
    poll 60 readyz 8089 || fail "yc-payment did not come back"
}

provision() {
    step "Provision tenant $TENANT ($SHOP_HOST, ZAR, VAT-inclusive)"
    # shellcheck source=lib/provision-tenant.sh
    source scripts/lib/provision-tenant.sh
    provision_tenant "$TENANT" "Demo Shop" "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"
    for path in /checkout /orders/mine /admin/gateway-config /admin/zones /admin/shipments /shipping/zones; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "checkout, orders, payment and shipping routes registered with the gateway"
}

shop_admin() {
    step "Shop admin: catalog, stock and Stripe"
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

    product() { # product <name> <price minor> <is_digital> <tracked> [sku json fields] → sets SKU
        call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"$1 $RUN\",\"description\":\"Phase 6 demo\",\"is_digital\":$3}"
        expect 201 "create $1"
        local id; id=$(jq -r .product_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\",\"inventory_tracked\":$4${5:-}}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY") SKU_BODY=$BODY
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000 false true ',"weight_grams":1500,"dimensions_mm":{"length":250,"width":200,"height":300}'
    KETTLE=$SKU
    jq -e '.weight_grams == 1500 and .dimensions_mm == {"length":250,"width":200,"height":300}' <<<"$SKU_BODY" >/dev/null ||
        fail "the Kettle's weight and dimensions were not stored"
    product E-book 9900 true false; EBOOK=$SKU
    ok "Kettle R200 (1.5 kg, 250 × 200 × 300 mm) and E-book R99 (digital)"

    has_stock() { call "$SHOP_HOST" "$ADMIN" GET "/admin/stock/$KETTLE"; [ "$STATUS" = 200 ]; }
    poll 15 has_stock || fail "yc-inventory has no stock row for the Kettle"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/stock/$KETTLE/receive" '{"quantity":5,"reason":"Phase 6 demo"}'
    expect 200 "receive Kettles"

    call "$SHOP_HOST" "$ADMIN" PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\"}"
    expect 200 "Stripe credentials"
    call "$SHOP_HOST" "$ADMIN" GET /admin/gateway-config
    expect 200 "gateway configs"
    for gw in $(jq -r '.items[] | select(.gateway != "stripe" and .enabled) | .gateway' <<<"$BODY"); do
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/gateway-config/$gw" '{"enabled":false}'
        expect 200 "turn $gw off"
    done
    ok "Stripe is the shop's gateway"
}

# zone <name> <priority> <match rules json> → sets ZONE
zone() {
    call "$SHOP_HOST" "$ADMIN" POST /admin/zones "{\"name\":\"$1\",\"priority\":$2,\"match_rules\":$3}"
    expect 201 "zone $1"
    ZONE=$(jq -r .zone_id <<<"$BODY")
}

# rate <json without zone_id>
rate() {
    call "$SHOP_HOST" "$ADMIN" POST /admin/rates "{\"zone_id\":\"$ZONE\",$1}"
    expect 201 "rate in $ZONE"
}

zones() {
    step "1. Shipping zones and rates"
    call "$SHOP_HOST" "$ADMIN" GET /admin/zones
    expect 200 "zones"
    for id in $(jq -r '.items[] | select(.name | IN("Gauteng", "Western Cape", "Rest of SA")) | .zone_id' <<<"$BODY"); do
        call "$SHOP_HOST" "$ADMIN" DELETE "/admin/zones/$id"
        expect 204 "replace zone $id"
    done
    zone Gauteng 10 '[{"country_code":"ZA","fields":{"province":"Gauteng"}}]'
    rate '"method":"standard","display_name":"Standard","pricing_type":"flat","flat_amount_minor":6000,"estimated_days_min":2,"estimated_days_max":3'
    rate '"method":"express","display_name":"Express","pricing_type":"flat","flat_amount_minor":12000,"estimated_days_min":1,"estimated_days_max":1'
    zone "Western Cape" 20 '[{"country_code":"ZA","fields":{"province":"Western Cape"}}]'
    rate '"method":"standard","display_name":"Standard","pricing_type":"weight_tiered","estimated_days_min":3,"estimated_days_max":5,
        "weight_tiers":[{"max_weight_g":1000,"amount_minor":7000},{"max_weight_g":5000,"amount_minor":9000},{"max_weight_g":20000,"amount_minor":16000}]'
    zone "Rest of SA" 90 '[{"country_code":"ZA"}]'
    rate '"method":"standard","display_name":"Standard","pricing_type":"price_tiered","estimated_days_min":4,"estimated_days_max":7,
        "price_tiers":[{"min_subtotal_minor":0,"amount_minor":15000},{"min_subtotal_minor":100000,"amount_minor":0}]'
    call "$SHOP_HOST" - GET /shipping/zones
    expect 200 "public zones"
    jq -r '.items[] | "    \(.name): \([.match_rules[] | .country_code + (if .fields then " " + (.fields | to_entries | map(.value) | join(", ")) else "" end)] | join("; "))"' <<<"$BODY"
    ok "Gauteng flat R60 / R120 express; Western Cape by weight; rest of SA R150, free from R1000"
}

addr() { # addr <street> <city> <province> <postal code>
    printf '{"country_code":"ZA","lines":["%s","%s %s"],"fields":{"street":"%s","city":"%s","province":"%s","postal_code":"%s"}}' \
        "$1" "$2" "$4" "$1" "$2" "$3" "$4"
}
JOBURG=$(addr "1 Jan Smuts Ave" Johannesburg Gauteng 2196)
CAPE_TOWN=$(addr "1 Long Street" "Cape Town" "Western Cape" 8001)
DURBAN=$(addr "1 Florida Road" Durban KwaZulu-Natal 4001)

customer() {
    local jar=$TMP/customer.jar
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"shopper+$RUN@demo-shop.test\",\"password\":\"shopper-password-$RUN\"}"
    expect 201 "register"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    added() { call "$SHOP_HOST" "$CUSTOMER" POST /carts/shopping/items "{\"sku_id\":\"$1\",\"quantity\":$2}"; [ "$STATUS" = 200 ]; }
    poll 15 added "$KETTLE" 1 || { echo "$BODY"; fail "add the Kettle"; }
    added "$EBOOK" 1 || fail "add the E-book"
}

ship() { # ship <address json> [option]
    local body="{\"delivery_address\":$1"
    [ -z "${2:-}" ] || body+=",\"shipping_option_id\":\"$2\""
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/shipping" "$body}"
}

checkout() {
    customer
    step "2. Options follow the delivery address"
    call "$SHOP_HOST" "$CUSTOMER" POST /checkout '{}'
    expect 201 "start checkout"
    SESSION=$(jq -r .session_id <<<"$BODY")
    jq -e '.requires_shipping and (.shipping_options | length) == 0' <<<"$BODY" >/dev/null ||
        fail "want a quote that needs shipping and has no options before an address"
    info "Kettle and E-book, no address yet: no options"

    ship "$JOBURG"; expect 200 "Johannesburg"
    info "Johannesburg (Gauteng):"; options
    jq -e '[.shipping_options[].id] == ["standard", "express"] and .totals.shipping.amount_minor == 6000' <<<"$BODY" >/dev/null ||
        fail "want Gauteng's Standard R60 and Express R120, Standard selected"
    ship "$CAPE_TOWN"; expect 200 "Cape Town"
    info "Cape Town (Western Cape, 1.5 kg):"; options
    [ "$(jq .totals.shipping.amount_minor <<<"$BODY")" = 9000 ] || fail "want the 1-5 kg tier, R90"
    ship "$DURBAN"; expect 200 "Durban"
    info "Durban (rest of SA, items R299):"; options
    [ "$(jq .totals.shipping.amount_minor <<<"$BODY")" = 15000 ] || fail "want R150 below R1000"
    ship '{"country_code":"NA","lines":["1 Independence Ave","Windhoek"]}'
    expect 409 "Windhoek"
    info "Windhoek: $(jq -r .detail <<<"$BODY")"
    ok "yc-shipping matched each address to a zone: flat, by weight and by items total; no zone, no options"

    step "3. Standard shipping to Johannesburg is in the totals"
    ship "$JOBURG" express; expect 200 "express"
    ship "$JOBURG" standard; expect 200 "standard"
    options
    jq -e '.selected_shipping_option_id == "standard" and .totals.total.amount_minor == 35900' <<<"$BODY" >/dev/null ||
        fail "want R299 + R60 = R359"
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/confirm" \
        "{\"delivery_address\":$CAPE_TOWN,\"payment_gateway_token\":\"pm_card_visa\"}" -H "Idempotency-Key: $(uuid)"
    expect 409 "confirm to another address"
    info "confirm to Cape Town instead: $(jq -r .detail <<<"$BODY")"
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/confirm" '{"payment_gateway_token":"pm_card_visa"}' \
        -H "Idempotency-Key: $(uuid)"
    expect 200 "confirm"
    [ "$(jq -r .state <<<"$BODY")" = COMPLETED ] || fail "want COMPLETED, got $(jq -r .state <<<"$BODY")"
    ORDER_ID=$(jq -r .order.order_id <<<"$BODY")
    ORDER_NUMBER=$(jq -r .order.order_number <<<"$BODY")
    ok "paid R359 (untaxed shipping R60); order $ORDER_NUMBER"

    step "4. The order has the shipping amount"
    call "$SHOP_HOST" "$CUSTOMER" GET "/orders/$ORDER_ID"
    expect 200 "order"
    jq -r "$JQ_MONEY"'"    \(.order_number)  \(.status)  items \(.items_total | amt)  shipping \(.shipping | amt) (\(.shipping_method))  total \(.total | amt)",
        "    to: \(.delivery_address.lines | join(", "))"' <<<"$BODY"
    jq -e '.shipping.amount_minor == 6000 and .shipping_method == "standard" and .status == "received"' <<<"$BODY" >/dev/null ||
        fail "want shipping R60 by standard"
    ok "order $ORDER_NUMBER: received, R60 Standard shipping to Johannesburg"
}

status_is() { call "$SHOP_HOST" "$CUSTOMER" GET "/orders/$ORDER_ID"; [ "$(jq -r .status <<<"$BODY")" = "$1" ]; }

# update <status> [json fields]: the admin sets the shipment's status.
update() {
    call "$SHOP_HOST" "$ADMIN" POST "/admin/shipments/$SHIPMENT/manual-update" "{\"status\":\"$1\"${2:-}}"
    expect 200 "shipment $1"
}

fulfil() {
    step "5. The admin packages the order: the shipment is booked"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/orders/$ORDER_ID/package"
    expect 200 "package"
    SHIPMENT=$(jq -r .shipment.shipment_id <<<"$BODY")
    TRACKING=$(jq -r .shipment.tracking_number <<<"$BODY")
    [ "$(jq -r .status <<<"$BODY")" = packaged ] || fail "want packaged"
    info "tracking number $TRACKING (manual carrier driver)"
    call "$SHOP_HOST" - GET "/shipping/tracking/$TRACKING"
    expect 200 "public tracking"
    info "anyone with the number sees: $(jq -r '.status + " by " + .method' <<<"$BODY")"
    ok "yc-orders booked shipment $SHIPMENT with yc-shipping; packaged"

    step "6. The admin marks it in transit: the order is shipped"
    update in_transit ',"carrier_name":"The Courier Guy","carrier_reference":"TCG-'"$RUN"'"'
    poll 30 status_is shipped || fail "shipping.in_transit.v1 did not ship the order"
    info "carrier $(jq -r '.shipment.carrier_name + " " + .shipment.carrier_reference' <<<"$BODY")"
    ok "shipping.in_transit.v1 → order shipped"

    step "7. The admin marks it delivered"
    update delivered
    poll 30 status_is delivered || fail "shipping.delivered.v1 did not deliver the order"
    ok "shipping.delivered.v1 → order delivered"

    step "8. The customer saw every step"
    call "$SHOP_HOST" "$CUSTOMER" GET /orders/mine
    expect 200 "my orders"
    jq -r "$JQ_MONEY"'.items[] | "    \(.order_number)  \(.status)  \(.total | amt)"' <<<"$BODY" | head -3
    call "$SHOP_HOST" "$CUSTOMER" GET "/orders/$ORDER_ID"
    local path; path=$(jq -r '[.history[] | .to_status] | join(" → ")' <<<"$BODY")
    info "$path"
    [[ $path == *"received → packaged → shipped → delivered" ]] || fail "unexpected status path"
    call "$SHOP_HOST" - GET "/shipping/tracking/$TRACKING"
    jq -r '.events[] | "    \(.occurred_at)  \(.status)\(if .message then "  " + .message else "" end)"' <<<"$BODY"
    ok "/orders/mine shows $ORDER_NUMBER delivered; history and tracking show each transition"
}

returns() {
    step "9. Return: the customer asks, the admin accepts, the money goes back"
    call "$SHOP_HOST" "$CUSTOMER" POST "/orders/$ORDER_ID/returns" \
        "{\"items\":[{\"sku_id\":\"$EBOOK\",\"quantity\":1}],\"reason\":\"Changed my mind\"}"
    expect 400 409 "return the E-book"
    info "the E-book: $(jq -r '.errors[0].message // .detail' <<<"$BODY")"
    call "$SHOP_HOST" "$CUSTOMER" POST "/orders/$ORDER_ID/returns" \
        "{\"items\":[{\"sku_id\":\"$KETTLE\",\"quantity\":1}],\"reason\":\"Arrived dented\"}"
    expect 201 "request return"
    local rid; rid=$(jq -r .return_id <<<"$BODY")
    info "requested: Kettle × 1, \"Arrived dented\""
    call "$SHOP_HOST" "$ADMIN" GET "/admin/returns?status=requested"
    expect 200 "admin returns"
    jq -e --arg id "$rid" '.items[] | select(.return_id == $id)' <<<"$BODY" >/dev/null || fail "the admin does not see the request"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/returns/$rid/accept" '{"refund_shipping":true,"note":"Sorry about that"}'
    expect 200 "accept"
    info "accepted: refund $(rand "$(jq .refund <<<"$BODY")") (the Kettle R200 and its R60 shipping)"
    [ "$(jq .refund <<<"$BODY")" = 26000 ] || fail "want R260 refunded"
    call "$SHOP_HOST" "$CUSTOMER" GET "/orders/$ORDER_ID"
    jq -r "$JQ_MONEY"'"    order \(.status), refunded \(.refunded | amt)",
        (.refunds[] | "    refund \(.amount | amt) \(.status): \(.reason)")' <<<"$BODY"
    jq -e '.status == "returned" and .refunded.amount_minor == 26000' <<<"$BODY" >/dev/null ||
        fail "want the order returned with R260 refunded"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/transactions?session_id=$SESSION"
    expect 200 "transactions"
    info "payment $(jq -r '.items[0] | .status' <<<"$BODY") on $(jq -r '.items[0].gateway' <<<"$BODY")"
    ok "every physical line came back, so the order is returned; yc-payment refunded R260 of R359"
}

digital() {
    step "10. An all-digital order skips shipping"
    local visitor=jar:$TMP/visitor.jar
    call "$SHOP_HOST" "$visitor" GET /me
    added() { call "$SHOP_HOST" "$visitor" POST /carts/shopping/items "{\"sku_id\":\"$EBOOK\",\"quantity\":1}"; [ "$STATUS" = 200 ]; }
    poll 15 added || fail "add the E-book"
    call "$SHOP_HOST" "$visitor" POST /checkout '{}'
    expect 201 "start"
    SESSION=$(jq -r .session_id <<<"$BODY")
    jq -e '(.requires_shipping | not) and (.shipping_options | length) == 0' <<<"$BODY" >/dev/null || fail "want no shipping"
    call "$SHOP_HOST" "$visitor" POST "/checkout/$SESSION/shipping" "{\"delivery_address\":$JOBURG}"
    expect 400 "ship an e-book"
    call "$SHOP_HOST" "$visitor" POST "/checkout/$SESSION/confirm" \
        "{\"payment_gateway_token\":\"pm_card_visa\",\"guest_contact\":{\"email\":\"guest+$RUN@demo-shop.test\",\"phone_e164\":\"+27831234567\"}}" \
        -H "Idempotency-Key: $(uuid)"
    expect 200 "confirm without an address"
    jq -r "$JQ_MONEY"'"    \(.order.order_number)  \(.state)  shipping \(.totals.shipping | amt)  total \(.totals.total | amt)"' <<<"$BODY"
    local id; id=$(jq -r .order.order_id <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" POST "/admin/orders/$id/package"
    expect 409 "package a digital order"
    info "packaging it: $(jq -r .detail <<<"$BODY")"
    ok "no address, no shipping step, nothing to package"
}

healthy
payments
provision
shop_admin
zones
checkout
fulfil
returns
digital
printf '\n\033[32m✓ Phase 6 milestone complete.\033[0m\n'
