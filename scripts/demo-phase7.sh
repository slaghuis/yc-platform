#!/usr/bin/env bash
# Phase 7 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 7 — Reviews, Preferences, Messaging").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase7.sh
#
# A customer registers and gets a welcome email and a verification code,
# which verifies their email channel (yc-preference → yc-message). They
# like products, order a Kettle and get the order confirmation, shipped and
# delivered emails, then a review prompt. Their review waits for the shop
# admin's approval before it shows on the product, and the rating, likes
# and the customer's favourites reach yc-product's search. A Teapot left
# in the cart brings the abandoned-cart email, and a password reset link
# arrives by email and works. Every email is read from MailHog's API.
#
# The demo restarts yc-cart (abandoned after 1 minute) and yc-review
# (review prompts 15 seconds after delivery) with short thresholds; run
# `docker compose --profile app up -d yc-cart yc-review` to restore the
# defaults. Payments use Stripe test mode when STRIPE_API_KEY (sk_test_…)
# is set in the environment or .env; otherwise the fake Stripe on the host.
# Needs curl, jq, perl, docker and go. Safe to re-run: each run uses a
# fresh customer and products.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
MAILHOG=${MAILHOG_URL:-http://localhost:8025}
TENANT=${TENANT:-demo-shop}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
RUN=$(date +%s)
EMAIL="shopper+$RUN@demo-shop.test"
PASSWORD="shopper-password-$RUN"

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

uuid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/'; }
readyz() { curl -fsS "http://localhost:$1/readyz" >/dev/null; }
routed() { ! curl -s -H "Host: $SHOP_HOST" "$GATEWAY$1" | grep -q 'Route not found'; }

# mail <to> <subject>: the decoded body of the newest email to <to> whose
# subject contains <subject>, from MailHog; fails when there is none.
mail() {
    local to subject=$2
    to=$(jq -rn --arg v "$1" '$v | @uri')
    curl -fsS "$MAILHOG/api/v2/search?kind=to&query=$to&limit=50" |
        jq -er --arg s "$subject" '[.items[] | select(.Content.Headers.Subject[0] | contains($s))][0].Content.Body' |
        perl -pe 's/=\r?\n//g; s/=([0-9A-F]{2})/chr(hex($1))/ge; s/\r//g'
}
# show_mail <to> <subject>: print the email, indented.
show_mail() {
    info "┌ To: $1"
    info "│ Subject: $2…"
    mail "$1" "$2" | sed '/^$/d' | head -"${3:-8}" | sed 's/^/    │ /'
}
# await_mail <seconds> <to> <subject> <what>
await_mail() {
    poll "$1" mail "$2" "$3" || fail "no \"$3\" email to $2 in MailHog ($4)"
}

healthy() {
    step "0. Services are up"
    for port in 8090 8081 8083 8084 8086 8087 8088 8089 8092 8093 8094 8095 8096; do
        readyz "$port" || fail "localhost:$port/readyz (task devstack:app)"
    done
    curl -fsS "$MAILHOG/api/v2/messages?limit=1" >/dev/null || fail "MailHog at $MAILHOG"
    ok "gateway, identity, product, pricing, preference, cart, inventory, payment, orders, checkout, shipping, message and review are ready; MailHog too"
}

short_thresholds() {
    step "Short thresholds for the demo"
    YC_CART_ABANDONED_AFTER=1m YC_CART_ABANDONED_SWEEP_INTERVAL=5s \
        YC_REVIEW_PROMPT_DELAY=15s YC_REVIEW_SWEEP_INTERVAL=5s \
        docker compose --profile app up -d --no-build yc-cart yc-review 2>&1 | sed 's/^/    /'
    poll 60 readyz 8087 || fail "yc-cart did not come back"
    poll 60 readyz 8096 || fail "yc-review did not come back"
    ok "carts are abandoned after 1 minute idle (24h by default); review prompts 15 seconds after delivery (72h)"
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
    go run ./tools/yc-tenant-cli provision --tenant-id "$TENANT" --name "Demo Shop" --hostname "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"
    for path in /reviews /likes /admin/reviews /admin/messages /admin/abandoned-carts /preferences/me; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "review, message, abandoned-cart and preference routes registered with the gateway"
}

shop_admin() {
    step "Shop admin: catalog, shipping, Stripe and manual review moderation"
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

    product() { # product <name> <price minor> → sets PRODUCT and SKU
        call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"$1 $RUN\",\"description\":\"Phase 7 demo\",\"is_digital\":false}"
        expect 201 "create $1"
        PRODUCT=$(jq -r .product_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$PRODUCT/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\",\"inventory_tracked\":false,\"weight_grams\":1000}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY")
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000; KETTLE=$SKU KETTLE_PRODUCT=$PRODUCT
    product Teapot 15000; TEAPOT=$SKU TEAPOT_PRODUCT=$PRODUCT
    ok "Kettle R200 and Teapot R150"

    call "$SHOP_HOST" "$ADMIN" GET /admin/zones
    expect 200 "zones"
    if [ "$(jq '[.items[] | select(.match_rules[] | .country_code == "ZA")] | length' <<<"$BODY")" = 0 ]; then
        call "$SHOP_HOST" "$ADMIN" POST /admin/zones '{"name":"South Africa","priority":90,"match_rules":[{"country_code":"ZA"}]}'
        expect 201 "zone"
        call "$SHOP_HOST" "$ADMIN" POST /admin/rates "{\"zone_id\":\"$(jq -r .zone_id <<<"$BODY")\",\"method\":\"standard\",
            \"display_name\":\"Standard\",\"pricing_type\":\"flat\",\"flat_amount_minor\":6000,\"estimated_days_min\":2,\"estimated_days_max\":5}"
        expect 201 "rate"
    fi
    ok "the shop ships within South Africa"

    call "$SHOP_HOST" "$ADMIN" PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\"}"
    expect 200 "Stripe credentials"
    call "$SHOP_HOST" "$ADMIN" GET /admin/gateway-config
    expect 200 "gateway configs"
    for gw in $(jq -r '.items[] | select(.gateway != "stripe" and .enabled) | .gateway' <<<"$BODY"); do
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/gateway-config/$gw" '{"enabled":false}'
        expect 200 "turn $gw off"
    done
    call "$SHOP_HOST" "$ADMIN" PUT /admin/review-settings '{"moderation_mode":"manual","require_purchase":true,"allow_edit":true}'
    expect 200 "review settings"
    ok "Stripe is the shop's gateway; reviews need a purchase and the admin's approval"
}

register() {
    step "1. A customer registers: welcome email"
    local jar=$TMP/customer.jar
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}"
    expect 201 "register"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    USER_ID=$(jq -r .user_id <<<"$BODY")
    await_mail 30 "$EMAIL" "Welcome to the shop" "identity.user.registered.v1 → yc-message"
    show_mail "$EMAIL" "Welcome to the shop"
    ok "identity.user.registered.v1 → yc-message → SMTP → MailHog"

    step "   The email channel is verified with the code yc-message sent"
    await_mail 30 "$EMAIL" "Your verification code" "preference.channel.added.v1 → yc-message"
    local code
    code=$(mail "$EMAIL" "Your verification code" | grep -oE '[0-9]{6}' | head -1)
    info "code from MailHog: $code"
    call "$SHOP_HOST" "$CUSTOMER" POST /preferences/me/channels/email/verify '{"code":"000000"}'
    [ "$code" = 000000 ] || expect 400 "a wrong code"
    info "a wrong code: $(jq -r '.errors[0].message // .detail' <<<"$BODY")"
    call "$SHOP_HOST" "$CUSTOMER" POST /preferences/me/channels/email/verify "{\"code\":\"$code\"}"
    expect 200 "verify"
    jq -e '.verified' <<<"$BODY" >/dev/null || fail "want the email channel verified"
    ok "yc-preference confirmed the code with yc-message: $(jq -r '.address' <<<"$BODY") verified"
}

likes() {
    step "2. The customer likes the Kettle and the Teapot"
    call "$SHOP_HOST" "$CUSTOMER" POST /reviews "{\"product_id\":\"$KETTLE_PRODUCT\",\"rating\":5}"
    expect 409 "review before buying"
    info "reviewing the Kettle before buying it: $(jq -r .detail <<<"$BODY")"
    for p in "$KETTLE_PRODUCT" "$TEAPOT_PRODUCT"; do
        call "$SHOP_HOST" "$CUSTOMER" PUT "/likes/$p"
        expect 200 "like"
    done
    call "$SHOP_HOST" "$CUSTOMER" GET /likes
    expect 200 "my likes"
    [ "$(jq '.items | length' <<<"$BODY")" = 2 ] || fail "want two likes"
    ok "two likes (review.like.added.v1); a review needs a purchase"
}

checkout() {
    step "3. The customer orders the Kettle: order confirmation email"
    added() { call "$SHOP_HOST" "$CUSTOMER" POST /carts/shopping/items "{\"sku_id\":\"$1\",\"quantity\":1}"; [ "$STATUS" = 200 ]; }
    poll 15 added "$KETTLE" || { echo "$BODY"; fail "add the Kettle"; }
    call "$SHOP_HOST" "$CUSTOMER" POST /checkout '{}'
    expect 201 "start checkout"
    SESSION=$(jq -r .session_id <<<"$BODY")
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/shipping" \
        '{"delivery_address":{"country_code":"ZA","lines":["1 Florida Road","Durban 4001"],"fields":{"street":"1 Florida Road","city":"Durban","province":"KwaZulu-Natal","postal_code":"4001"}}}'
    expect 200 "address"
    call "$SHOP_HOST" "$CUSTOMER" POST "/checkout/$SESSION/confirm" '{"payment_gateway_token":"pm_card_visa"}' \
        -H "Idempotency-Key: $(uuid)"
    expect 200 "confirm"
    [ "$(jq -r .state <<<"$BODY")" = COMPLETED ] || fail "want COMPLETED, got $(jq -r .state <<<"$BODY")"
    ORDER_ID=$(jq -r .order.order_id <<<"$BODY")
    ORDER_NUMBER=$(jq -r .order.order_number <<<"$BODY")
    await_mail 30 "$EMAIL" "Order $ORDER_NUMBER confirmed" "order.placed.v1 → yc-message"
    show_mail "$EMAIL" "Order $ORDER_NUMBER confirmed" 12
    ok "order.placed.v1 → order confirmation with the lines and totals"

    step "   The Teapot stays in the cart (abandoned in a minute)"
    poll 15 added "$TEAPOT" || fail "add the Teapot"
    ok "Teapot in the shopping cart since $(date +%T)"
}

favourites() {
    step "4. The Kettle is in the customer's favourites"
    fav() {
        call "$SHOP_HOST" "$CUSTOMER" GET "/products?favourites=true"
        jq -e --arg n "Kettle $RUN" '[.items[].name] == [$n] and .facets.favourites.product_count >= 1' <<<"$BODY"
    }
    poll 15 fav || { echo "$BODY" | head -c 600; fail "order.placed.v1 did not reach the favourites"; }
    info "GET /products?favourites=true: $(jq -r '[.items[].name] | join(", ")' <<<"$BODY") (facet: $(jq .facets.favourites.product_count <<<"$BODY") product)"
    call "$SHOP_HOST" - GET "/products?favourites=true"
    expect 400 401 403 "favourites without signing in"
    ok "order.placed.v1 → yc-product favourites; the Teapot, liked but not bought, is not a favourite"
}

status_is() { call "$SHOP_HOST" "$CUSTOMER" GET "/orders/$ORDER_ID"; [ "$(jq -r .status <<<"$BODY")" = "$1" ]; }
update() {
    call "$SHOP_HOST" "$ADMIN" POST "/admin/shipments/$SHIPMENT/manual-update" "{\"status\":\"$1\"${2:-}}"
    expect 200 "shipment $1"
}

fulfil() {
    step "5. The admin ships the order: shipped and delivered emails"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/orders/$ORDER_ID/package"
    expect 200 "package"
    SHIPMENT=$(jq -r .shipment.shipment_id <<<"$BODY")
    update in_transit ',"carrier_name":"The Courier Guy","carrier_reference":"TCG-'"$RUN"'"'
    poll 30 status_is shipped || fail "the order was not shipped"
    await_mail 30 "$EMAIL" "Order $ORDER_NUMBER is on its way" "order.shipped.v1 → yc-message"
    show_mail "$EMAIL" "Order $ORDER_NUMBER is on its way"
    update delivered
    poll 30 status_is delivered || fail "the order was not delivered"
    await_mail 30 "$EMAIL" "Order $ORDER_NUMBER was delivered" "order.delivered.v1 → yc-message"
    ok "order.shipped.v1 and order.delivered.v1 → emails with tracking"

    step "   yc-review asks for a review after delivery"
    await_mail 60 "$EMAIL" "How was your order $ORDER_NUMBER" "review.prompt_requested.v1 → yc-message"
    show_mail "$EMAIL" "How was your order $ORDER_NUMBER"
    ok "order.delivered.v1 → yc-review (15s later) → review.prompt_requested.v1 → yc-message"
}

reviews() {
    step "6. The customer reviews the Kettle; it shows once approved"
    call "$SHOP_HOST" "$CUSTOMER" POST /reviews \
        "{\"product_id\":\"$KETTLE_PRODUCT\",\"rating\":5,\"title\":\"Boils fast\",\"body\":\"Quiet, and the handle stays cool.\"}"
    expect 201 "review"
    local id; id=$(jq -r .review_id <<<"$BODY")
    jq -e '.status == "pending" and .verified_purchase' <<<"$BODY" >/dev/null || fail "want a pending, verified-purchase review"
    info "submitted: ★★★★★ \"Boils fast\", pending, verified purchase (yc-orders)"
    call "$SHOP_HOST" - GET "/reviews?product_id=$KETTLE_PRODUCT"
    expect 200 "public reviews"
    [ "$(jq '.items | length' <<<"$BODY")" = 0 ] || fail "a pending review is not public"
    info "the product shows no reviews yet"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/reviews?status=pending&product_id=$KETTLE_PRODUCT"
    expect 200 "moderation queue"
    jq -e --arg id "$id" '.items[] | select(.review_id == $id)' <<<"$BODY" >/dev/null || fail "the admin does not see it"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/reviews/$id/approve"
    expect 200 "approve"
    call "$SHOP_HOST" "$ADMIN" PUT "/admin/reviews/$id/response" '{"response":"Thank you, enjoy your tea!"}'
    expect 200 "respond"
    call "$SHOP_HOST" - GET "/reviews?product_id=$KETTLE_PRODUCT"
    expect 200 "public reviews"
    jq -r '.items[] | "    ★\(.rating) \(.title) — \(.body)  (verified purchase: \(.verified_purchase))", "      shop: \(.shop_response)"' <<<"$BODY"
    jq -e '(.items | length) == 1 and (.items[0] | has("user_id") | not)' <<<"$BODY" >/dev/null ||
        fail "want the approved review, without its author's ID"
    call "$SHOP_HOST" - GET "/reviews/stats?product_id=$KETTLE_PRODUCT"
    expect 200 "stats"
    info "stats: $(jq -c '{rating_count, rating_average, like_count, distribution}' <<<"$BODY")"
    ok "pending → approved by the admin → public, with the shop's response"

    step "   Ratings and likes reach the product search"
    rated() {
        call "$SHOP_HOST" - GET "/products?sort=rating&page_size=5"
        jq -e --arg n "Kettle $RUN" '.items[0] | .name == $n and .reviews.rating_average == 5 and .reviews.like_count == 1' <<<"$BODY"
    }
    poll 15 rated || { echo "$BODY" | head -c 600; fail "review.approved.v1 did not reach yc-product"; }
    jq -r '.items[:3][] | "    \(.name)  rating \(.reviews.rating_average // "-") (\(.reviews.rating_count))  likes \(.reviews.like_count)"' <<<"$BODY"
    ok "GET /products?sort=rating puts the Kettle first; the Teapot has a like and no rating"
}

abandoned() {
    step "7. The Teapot sat in the cart for a minute: abandoned-cart email"
    await_mail 120 "$EMAIL" "You left something in your cart" "cart.abandoned.v1 → yc-message"
    show_mail "$EMAIL" "You left something in your cart"
    call "$SHOP_HOST" "$ADMIN" GET /admin/abandoned-carts
    expect 200 "abandoned carts"
    jq -e --arg u "$USER_ID" '.items[] | select(.user_id == $u and .item_count == 1)' <<<"$BODY" >/dev/null ||
        fail "the admin does not see the cart"
    ok "yc-cart's sweeper → cart.abandoned.v1 (once per idle spell) → yc-message; listed for the admin"
}

reset() {
    step "8. Password reset by email"
    call "$SHOP_HOST" - POST /password/reset-request "{\"email\":\"$EMAIL\"}"
    expect 202 "reset request"
    await_mail 30 "$EMAIL" "Reset your password" "yc-identity → yc-message /internal/send"
    local link token
    link=$(mail "$EMAIL" "Reset your password" | grep -oE 'https?://[^ ]+token=[^ ]+' | head -1)
    info "link: ${link%%token=*}token=…"
    token=${link##*token=}
    token=$(printf '%b' "${token//%/\\x}")
    call "$SHOP_HOST" - POST /password/reset-confirm "{\"token\":\"$token\",\"password\":\"new-$PASSWORD\"}"
    expect 204 "reset"
    call "$SHOP_HOST" - POST /login "{\"email\":\"$EMAIL\",\"password\":\"new-$PASSWORD\"}"
    expect 200 "login with the new password"
    call "$SHOP_HOST" - POST /password/reset-request '{"email":"nobody@demo-shop.test"}'
    expect 202 "unknown email"
    ok "the emailed link set a new password; an unknown email gets the same 202"
}

history() {
    step "9. The shop's message history"
    call "$SHOP_HOST" "$ADMIN" GET "/admin/messages?user_id=$USER_ID&page_size=20"
    expect 200 "messages"
    jq -r '.items | reverse[] | "    \(.type)  \(.channel) → \(.address)  \(.status)\(if .body == "[redacted]" then "  (body redacted)" else "" end)"' <<<"$BODY"
    jq -e '[.items[] | select(.status == "sent")] | length >= 8' <<<"$BODY" >/dev/null || fail "want every message sent"
    ok "every message was sent; reset links and codes are not kept"
}

healthy
short_thresholds
payments
provision
shop_admin
register
likes
checkout
favourites
fulfil
reviews
abandoned
reset
history
printf '\n\033[32m✓ Phase 7 milestone complete.\033[0m\n'
info "Restore the default thresholds: docker compose --profile app up -d yc-cart yc-review"
