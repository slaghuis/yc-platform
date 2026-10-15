#!/usr/bin/env bash
# Phase 8 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 8 — Operational Maturity").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase8.sh
#
# 1. Every service is registered in yc-monitor and the dashboard is green.
# 2. A paused service is marked unhealthy, a critical alert opens and the
#    system admins get an email; it recovers when resumed.
# 3. A new shop takes orders: the shop admin's analytics dashboard shows
#    revenue, orders, top products and the funnel, and a first-order
#    milestone email arrives.
# 4. A Stripe chargeback becomes payment.chargeback_received.v1, then
#    order.disputed.v1, then a shop alert and an email to the shop admin.
# 5. With yc-preference paused, 24 abandoned-cart events dead-letter in
#    yc-message; the monitor alerts on the spike and yc-dlq-cli replays them
#    once yc-preference is back.
# 6. A customer deletes their account: the GDPR register completes and their
#    order and analytics facts carry a pseudonym.
# 7. Exit criteria: audit records cannot be changed (database privileges)
#    and the analytics aggregates match their facts.
#
# The demo restarts yc-monitor, yc-message, yc-analytics and yc-payment with
# short intervals and the fake Stripe on the host; run
# `docker compose --profile app up -d` to restore the defaults. Needs curl,
# jq, perl, docker and go. Safe to re-run: each run provisions a new shop.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
MAILHOG=${MAILHOG_URL:-http://localhost:8025}
RUN=$(date +%s)
TENANT=${TENANT:-ops-$RUN}
SHOP_HOST=${SHOP_HOST:-$TENANT.localhost}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
OPS_EMAIL=${OPS_EMAIL:-ops@yourcommerce.test}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
WEBHOOK_SECRET=whsec_phase8_demo
# dlq_rate alerts above 10 dead letters a minute, counted per clock minute,
# so the batch must clear 10 even when it straddles one.
ABANDONED=24
export NATS_URL=${NATS_URL:-nats://localhost:4222}

TMP=$(mktemp -d)
FAKE_PID=
PAUSED=()
cleanup() {
    for c in ${PAUSED[@]+"${PAUSED[@]}"}; do docker unpause "$c" >/dev/null 2>&1 || true; done
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
        sleep 1
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
routed() { ! curl -s -H "Host: ${2:-$SHOP_HOST}" "$GATEWAY$1" | grep -q 'Route not found'; }

# Emails sent since this run started, so earlier runs' emails to the same
# address (the system admins') do not count.
SINCE=$(date -u +%Y-%m-%dT%H:%M:%S)

# mails <to> <subject>: this run's emails to <to> whose subject contains
# <subject>, newest first, as a JSON array.
mails() {
    local to
    to=$(jq -rn --arg v "$1" '$v | @uri')
    curl -fsS "$MAILHOG/api/v2/search?kind=to&query=$to&limit=500" |
        jq -c --arg s "$2" --arg since "$SINCE" \
            '[.items[] | select(.Created >= $since and (.Content.Headers.Subject[0] | contains($s)))]'
}
mail_count() { mails "$1" "$2" | jq length; }
# mail <to> <subject>: the decoded body of the newest such email; fails when
# there is none.
mail() {
    mails "$1" "$2" | jq -er '.[0].Content.Body' |
        perl -pe 's/=\r?\n//g; s/=([0-9A-F]{2})/chr(hex($1))/ge; s/\r//g'
}
show_mail() {
    info "┌ To: $1"
    info "│ Subject: $(mails "$1" "$2" | jq -r '.[0].Content.Headers.Subject[0]')"
    mail "$1" "$2" | sed '/^$/d' | head -"${3:-6}" | sed 's/^/    │ /'
}
await_mail() {
    poll "$1" mail "$2" "$3" || fail "no \"$3\" email to $2 in MailHog ($4)"
}

sys() { call "$ADMIN_HOST" "$SYS" "$@"; }
shop() { call "$SHOP_HOST" "$ADMIN" "$@"; }

dashboard_line() {
    sys GET /sysadmin/dashboard
    jq -r '"status \(.status): \(.services.healthy)/\(.services.total) healthy, \(.services.unhealthy) unhealthy, \(.services.unknown) unknown; alerts \(.open_alerts.critical) critical, \(.open_alerts.warning) warning; \(.dead_letters_unacknowledged) dead letters"' <<<"$BODY"
}
dashboard_is() { sys GET /sysadmin/dashboard; [ "$(jq -r .status <<<"$BODY")" = "$1" ]; }
service_is() {
    sys GET /sysadmin/services
    jq -e --arg n "$1" --arg s "$2" '.items[] | select(.name == $n and .status == $s)' <<<"$BODY"
}
# alert_is <rule> <status> [service]: the newest alert for the rule (and
# monitored service) has the status; sets ALERT to it.
alert_is() {
    sys GET "/sysadmin/alerts?rule=$1&page_size=50"
    ALERT=$(jq -c --arg s "${3:-}" '[.items[] | select($s == "" or .dimensions.service == $s)][0] // empty' <<<"$BODY")
    [ -n "$ALERT" ] && [ "$(jq -r .status <<<"$ALERT")" = "$2" ]
}

healthy() {
    step "0. Services are up"
    for port in 8090 8081 8082 8083 8084 8085 8086 8087 8088 8089 8092 8093 8094 8095 8096 8097 8098; do
        readyz "$port" || fail "localhost:$port/readyz (task devstack:app)"
    done
    curl -fsS "$MAILHOG/api/v2/messages?limit=1" >/dev/null || fail "MailHog at $MAILHOG"
    ok "gateway, identity and all 15 registered services are ready, monitor, audit and analytics included"
}

short_intervals() {
    step "Short intervals for the demo"
    STRIPE_API_KEY=sk_test_fake_demo
    go build -o "$TMP/fake-stripe" ./services/yc-payment/cmd/fake-stripe
    "$TMP/fake-stripe" -addr ":$FAKE_STRIPE_PORT" >"$TMP/fake-stripe.log" 2>&1 &
    FAKE_PID=$!
    poll 10 curl -s "http://localhost:$FAKE_STRIPE_PORT/v1/balance" || fail "fake Stripe did not start"
    YC_MONITOR_HEALTH_INTERVAL=3s YC_MONITOR_EVALUATE_INTERVAL=5s \
        YC_MESSAGE_EVENT_RETRY_BACKOFF=1s,1s,1s,1s YC_ANALYTICS_REFRESH_INTERVAL=3s \
        YC_PAYMENT_STRIPE_API_BASE=http://host.docker.internal:$FAKE_STRIPE_PORT \
        docker compose --profile app up -d --no-build yc-monitor yc-message yc-analytics yc-payment 2>&1 | sed 's/^/    /'
    for port in 8082 8095 8098 8089; do
        poll 60 readyz "$port" || fail "localhost:$port did not come back"
    done
    ok "health checks every 3s and alert rules every 5s (15s by default); yc-message redelivers failed events after 1s (1s, 5s, 30s…)"
    ok "analytics refreshes every 3s (30s); yc-payment uses the fake Stripe on localhost:$FAKE_STRIPE_PORT"
}

sysadmin() {
    docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
        /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
    call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
    expect 200 "system admin login"
    SYS=$(jq -r .access_token <<<"$BODY")
    for path in /sysadmin/dashboard /sysadmin/audit /sysadmin/analytics/platform; do
        poll 45 routed "$path" "$ADMIN_HOST" || fail "no gateway route for $path"
    done
}

monitored() {
    step "1. Every service is registered in yc-monitor; the dashboard is green"
    sysadmin
    all_healthy() { sys GET /sysadmin/dashboard; jq -e '.services.total > 0 and .services.healthy == .services.total' <<<"$BODY"; }
    poll 90 all_healthy || { dashboard_line | sed 's/^/    /'; fail "not every service is healthy"; }
    sys GET "/sysadmin/alerts?status=open&page_size=100"
    local id
    for id in $(jq -r '.items[] | select(.rule != "dlq_rate" and .rule != "checkout_success_rate" and .rule != "service_unhealthy") | .id' <<<"$BODY"); do
        info "earlier run's alert acknowledged: $(jq -r --arg id "$id" '.items[] | select(.id == $id) | "[\(.severity)] \(.summary)"' <<<"$BODY")"
        sys POST "/sysadmin/alerts/$id/ack"; expect 200 "acknowledge $id"
    done
    poll 90 dashboard_is green || { dashboard_line | sed 's/^/    /'; sys GET "/sysadmin/alerts?status=active"
        jq -r '.items[] | "    open: [\(.severity)] \(.summary)"' <<<"$BODY"; fail "the dashboard is not green"; }
    sys GET /sysadmin/services
    jq -r '.items[] | select(.status != "deregistered") | "    \(.name)\t\(.status)\t\(.base_url)"' <<<"$BODY" | column -t -s $'\t'
    info "$(dashboard_line)"
    ok "yc-gateway registrations → platform.service.registered.v1 → yc-monitor checks each /readyz"
}

forced_failure() {
    step "2. A forced failure: yc-review is paused"
    docker pause yc-review >/dev/null; PAUSED+=(yc-review)
    info "docker pause yc-review at $(date +%T) (a paused process does not shut down or deregister)"
    poll 60 service_is yc-review unhealthy || fail "yc-review was not marked unhealthy"
    info "$(jq -r '.items[] | select(.name == "yc-review") | "yc-review \(.status) after \(.consecutive_failures) failed checks"' <<<"$BODY")"
    poll 30 alert_is service_unhealthy open yc-review || fail "no service_unhealthy alert"
    info "alert: $(jq -r '"[\(.severity)] \(.summary)"' <<<"$ALERT")"
    info "$(dashboard_line)"
    await_mail 60 "$OPS_EMAIL" "yc-review failed" "monitor.alert.opened.v1 → yc-message admin_alert"
    show_mail "$OPS_EMAIL" "yc-review failed"
    ok "3 failed checks → monitor.service.unhealthy.v1 → critical alert → email to the system admins"

    step "   yc-review resumes"
    docker unpause yc-review >/dev/null; PAUSED=()
    poll 60 service_is yc-review healthy || fail "yc-review did not recover"
    poll 30 alert_is service_unhealthy resolved yc-review || fail "the alert was not resolved"
    poll 60 dashboard_is green || { dashboard_line; fail "the dashboard did not turn green"; }
    info "$(dashboard_line)"
    ok "3 good checks → monitor.service.recovered.v1 → alert resolved → green"
}

provision() {
    step "3. A new shop, $TENANT ($SHOP_HOST), takes orders"
    go run ./tools/yc-tenant-cli provision --tenant-id "$TENANT" --name "Ops Demo $RUN" --hostname "$SHOP_HOST" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true | sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"

    local jar=$TMP/owner.jar password="owner-password-$RUN" user_id
    OWNER_EMAIL="owner+$RUN@ops-demo.test"
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$OWNER_EMAIL\",\"password\":\"$password\"}"
    expect 201 "register owner"
    user_id=$(jq -r .user_id <<<"$BODY")
    sys POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote"
    call "$SHOP_HOST" - POST /login "{\"email\":\"$OWNER_EMAIL\",\"password\":\"$password\"}"
    expect 200 "shop admin login"
    ADMIN=$(jq -r .access_token <<<"$BODY")

    shop PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 200 "pricing config"
    shop PUT /admin/tax-classes/za-standard \
        '{"display_name":"Standard VAT","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0.15","base":"line_total","sort_order":0}]}'
    expect 200 201 "tax class"
    product() { # product <name> <price minor> → sets SKU
        shop POST /admin/products "{\"name\":\"$1\",\"description\":\"Phase 8 demo\",\"is_digital\":false}"
        expect 201 "create $1"
        local id; id=$(jq -r .product_id <<<"$BODY")
        shop POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"$(tr '[:lower:]' '[:upper:]' <<<"$1")-$RUN\",\"tax_class\":\"za-standard\",\"inventory_tracked\":false,\"weight_grams\":1000}"
        expect 201 "SKU $1"
        SKU=$(jq -r .sku_id <<<"$BODY")
        shop PUT "/admin/prices/sku/$SKU" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    product Kettle 20000; KETTLE=$SKU
    product Teapot 15000; TEAPOT=$SKU
    shop POST /admin/zones '{"name":"South Africa","priority":90,"match_rules":[{"country_code":"ZA"}]}'
    expect 201 "zone"
    shop POST /admin/rates "{\"zone_id\":\"$(jq -r .zone_id <<<"$BODY")\",\"method\":\"standard\",
        \"display_name\":\"Standard\",\"pricing_type\":\"flat\",\"flat_amount_minor\":6000,\"estimated_days_min\":2,\"estimated_days_max\":5}"
    expect 201 "rate"
    shop PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\",\"webhook_secret\":\"$WEBHOOK_SECRET\"}"
    expect 200 "Stripe credentials"
    for path in /admin/analytics/dashboard /admin/alerts /admin/audit; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "Kettle R200 and Teapot R150, shipping R60, Stripe (fake) with a webhook secret"
}

# customer <name> → sets CUSTOMER (token), CUSTOMER_ID and CUSTOMER_EMAIL
customer() {
    local jar=$TMP/$1.jar
    CUSTOMER_EMAIL="$1+$RUN@ops-demo.test"
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$CUSTOMER_EMAIL\",\"password\":\"password-$RUN\"}"
    expect 201 "register $1"
    CUSTOMER=$(jq -r .access_token <<<"$BODY")
    CUSTOMER_ID=$(jq -r .user_id <<<"$BODY")
}

# order <token> <sku...> → sets ORDER_ID and ORDER_NUMBER
order() {
    local token=$1 sku; shift
    for sku in "$@"; do
        added() { call "$SHOP_HOST" "$token" POST /carts/shopping/items "{\"sku_id\":\"$1\",\"quantity\":1}"; [ "$STATUS" = 200 ]; }
        poll 15 added "$sku" || { echo "$BODY"; fail "add to cart"; }
    done
    call "$SHOP_HOST" "$token" POST /checkout '{}'
    expect 201 "start checkout"
    local session; session=$(jq -r .session_id <<<"$BODY")
    call "$SHOP_HOST" "$token" POST "/checkout/$session/shipping" \
        '{"delivery_address":{"country_code":"ZA","lines":["1 Florida Road","Durban 4001"],"fields":{"street":"1 Florida Road","city":"Durban","province":"KwaZulu-Natal","postal_code":"4001"}}}'
    expect 200 "address"
    call "$SHOP_HOST" "$token" POST "/checkout/$session/confirm" '{"payment_gateway_token":"pm_card_visa"}' \
        -H "Idempotency-Key: $(uuid)"
    expect 200 "confirm"
    [ "$(jq -r .state <<<"$BODY")" = COMPLETED ] || fail "want COMPLETED, got $(jq -r .state <<<"$BODY")"
    ORDER_ID=$(jq -r .order.order_id <<<"$BODY")
    ORDER_NUMBER=$(jq -r .order.order_number <<<"$BODY")
}

analytics() {
    customer alice; ALICE=$CUSTOMER ALICE_ID=$CUSTOMER_ID ALICE_EMAIL=$CUSTOMER_EMAIL
    order "$ALICE" "$KETTLE" "$TEAPOT"; DISPUTED_ORDER=$ORDER_ID DISPUTED_NUMBER=$ORDER_NUMBER
    info "Alice: $ORDER_NUMBER (Kettle + Teapot)"
    customer bob; BOB=$CUSTOMER BOB_ID=$CUSTOMER_ID BOB_EMAIL=$CUSTOMER_EMAIL
    order "$BOB" "$KETTLE"; BOB_ORDER=$ORDER_ID
    info "Bob:   $ORDER_NUMBER (Kettle)"
    ok "two orders, two customers"

    step "   The shop's first order is a milestone"
    await_mail 60 "$OWNER_EMAIL" "Your shop reached a milestone" "analytics.milestone.reached.v1 → yc-message"
    show_mail "$OWNER_EMAIL" "Your shop reached a milestone" 3
    ok "order.placed.v1 → yc-analytics → analytics.milestone.reached.v1 → email to the shop admins"

    step "   The shop admin's analytics dashboard"
    counted() {
        shop GET /admin/analytics/dashboard
        jq -e '.pending_days == 0 and .revenue[0].orders == 2 and .funnel.orders_placed == 2' <<<"$BODY"
    }
    poll 60 counted || { echo "$BODY" | head -c 800; fail "the dashboard does not show both orders"; }
    jq -r '.revenue[] | "    revenue \(.currency): gross \(.gross_minor / 100), net \(.net_minor / 100) over \(.orders) orders (average \(.average_order_minor / 100))"' <<<"$BODY"
    jq -r '"    top products: " + ([.top_products[] | "\(.name) ×\(.units) (R\(.revenue_minor / 100))"] | join(", "))' <<<"$BODY"
    jq -r '.funnel | "    funnel: \(.visitors) visitors → \(.carts_created) carts → \(.checkouts_started) checkouts → \(.checkouts_confirmed) confirmed → \(.orders_placed) orders"' <<<"$BODY"
    jq -r '.gateways[] | "    \(.gateway): \(.successes)/\(.attempts) succeeded, R\(.amount_minor / 100)"' <<<"$BODY"
    call "$SHOP_HOST" "$ADMIN" GET /admin/analytics/export/top_products "" -D "$TMP/headers"
    expect 200 "CSV export"
    info "export: $(grep -i '^content-disposition' "$TMP/headers" | tr -d '\r')"
    sed 's/^/      /' <<<"$BODY"
    ok "facts from order, checkout, cart, payment and identity events → aggregates → dashboard and CSV"
}

chargeback() {
    step "4. A chargeback on $DISPUTED_NUMBER"
    shop GET "/admin/transactions?order_id=$DISPUTED_ORDER"
    expect 200 "transactions"
    local pi; pi=$(jq -r '.items[0].gateway_reference' <<<"$BODY")
    info "transaction $(jq -r '.items[0] | "\(.transaction_id) \(.status), Stripe \(.gateway_reference)"' <<<"$BODY")"
    curl -fsS -X POST "http://localhost:$FAKE_STRIPE_PORT/_fake/payment_intents/$pi/dispute" -H 'Content-Type: application/json' \
        -d "{\"webhook_url\":\"http://$SHOP_HOST:8090/webhooks/stripe\",\"webhook_secret\":\"$WEBHOOK_SECRET\",\"reason\":\"fraudulent\"}" \
        >"$TMP/dispute" || fail "fake Stripe dispute"
    [ "$(jq -r .status "$TMP/dispute")" = 200 ] || { cat "$TMP/dispute"; fail "the webhook was refused"; }
    info "fake Stripe: $(jq -r .dispute_id "$TMP/dispute") opened; signed charge.dispute.created delivered (200)"
    disputed() { shop GET "/admin/orders/$DISPUTED_ORDER"; [ "$(jq -r .status <<<"$BODY")" = disputed ]; }
    poll 30 disputed || fail "the order is not disputed"
    ok "yc-payment → payment.chargeback_received.v1 → yc-orders → order.disputed.v1"
    shop_alert() { shop GET "/admin/alerts?status=open"; jq -e '.items[] | select(.rule == "order_disputed")' <<<"$BODY"; }
    poll 30 shop_alert || fail "no order_disputed alert for the shop"
    info "shop alert: $(jq -r '[.items[] | select(.rule == "order_disputed")][0] | "[\(.severity)] \(.summary)"' <<<"$BODY")"
    await_mail 60 "$OWNER_EMAIL" "is disputed" "monitor.alert.opened.v1 → yc-message"
    show_mail "$OWNER_EMAIL" "is disputed" 4
    shop GET "/admin/audit?category=financial"
    expect 200 "audit"
    jq -r '.items[] | "    audit: \(.event_type): \(.summary)"' <<<"$BODY"
    ok "order.disputed.v1 → yc-monitor shop alert → email to the shop admin; recorded in yc-audit"
}

abandoned_event() { # abandoned_event <user_id> → an envelope on stdout
    jq -cn --arg id "$(uuid)" --arg t "$TENANT" --arg c "$(uuid)" --arg cart "$(uuid)" --arg u "$1" \
        --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
        event_id: $id, event_type: "cart.abandoned.v1", event_time: $now, tenant_id: $t,
        actor: {type: "service", id: "yc-cart"}, correlation_id: $c, source_service: "yc-cart",
        schema_version: 1, payload: {cart_id: $cart, user_id: $u, cart_type: "shopping", item_count: 1,
        quantity: 1, last_activity_at: $now, abandoned_at: $now}}'
}

dlq() {
    step "5. Dead letters: yc-preference is paused while abandoned-cart emails go out"
    docker pause yc-preference >/dev/null; PAUSED+=(yc-preference)
    poll 60 service_is yc-preference unhealthy || fail "yc-preference was not marked unhealthy"
    sleep 6 # the gateway also drops its routes after 3 failed checks (5s apart)
    info "yc-preference is unhealthy; yc-message cannot look up how to reach customers"
    local i since; since=$(date -u +%Y-%m-%dT%H:%M:%S)
    for i in $(seq 1 "$ABANDONED"); do abandoned_event "$BOB_ID"; done >"$TMP/events"
    docker run --rm -i --network yc-platform_default natsio/nats-box:latest sh -c \
        "while read -r e; do nats --server nats://nats:4222 pub -q 'yc.$TENANT.cart.abandoned.v1' \"\$e\" || exit 1; done" \
        <"$TMP/events" >/dev/null 2>&1 || fail "nats-box publish"
    ok "$ABANDONED cart.abandoned.v1 for Bob published on yc.$TENANT.cart.abandoned.v1"

    dead() {
        sys GET /sysadmin/dlq/summary
        jq -e --arg t "$TENANT" --argjson n "$ABANDONED" \
            '.items[] | select(.tenant_id == $t and .service == "yc-message" and .unacknowledged >= $n)' <<<"$BODY"
    }
    poll 120 dead || { echo "$BODY"; fail "the events were not dead-lettered"; }
    info "$(jq -r --arg t "$TENANT" '.items[] | select(.tenant_id == $t) | "\(.service): \(.unacknowledged) unacknowledged dead letters"' <<<"$BODY")"
    # The rule counts the current minute, so the alert may already have
    # resolved by the time every event is dead-lettered.
    dlq_alert() {
        sys GET "/sysadmin/alerts?rule=dlq_rate&page_size=10"
        ALERT=$(jq -c --arg s "$since" '[.items[] | select(.opened_at >= $s)][0] // empty' <<<"$BODY")
        [ -n "$ALERT" ]
    }
    dlq_resolved() { dlq_alert && [ "$(jq -r .status <<<"$ALERT")" = resolved ]; }
    poll 60 dlq_alert || fail "no dlq_rate alert"
    info "alert: $(jq -r '"[\(.severity)] \(.summary), opened \(.opened_at[11:19])"' <<<"$ALERT")"
    await_mail 60 "$OPS_EMAIL" "dead letters in the last" "monitor.alert.opened.v1 → yc-message"
    show_mail "$OPS_EMAIL" "dead letters in the last" 3
    ok "retries exhausted → yc.$TENANT.cart.dlq.v1 → yc-monitor counts the spike → alert and email"
    poll 120 dlq_resolved || fail "the dlq_rate alert did not resolve"
    info "resolved on its own at $(jq -r '.resolved_at[11:19]' <<<"$ALERT"), once a minute passed under 10 dead letters"

    step "   yc-dlq-cli lists them"
    go run ./tools/yc-dlq-cli list --tenant "$TENANT" --domain cart --service yc-message --limit 5 | sed 's/^/    /'

    step "   yc-preference resumes; yc-dlq-cli replays the dead letters"
    docker unpause yc-preference >/dev/null; PAUSED=()
    poll 60 service_is yc-preference healthy || fail "yc-preference did not recover"
    poll 60 routed /preferences/me || fail "yc-preference did not re-register its routes"
    go run ./tools/yc-dlq-cli replay --tenant "$TENANT" --domain cart --service yc-message --all | tail -3 | sed 's/^/    /'
    emailed() { [ "$(mail_count "$BOB_EMAIL" "You left something in your cart")" -ge "$ABANDONED" ]; }
    poll 90 emailed || fail "the replayed events did not send the emails ($(mail_count "$BOB_EMAIL" "You left something"))"
    ok "$ABANDONED abandoned-cart emails reached Bob after the replay"
    sys GET "/sysadmin/dlq?tenant_id=$TENANT&acknowledged=false&limit=100"
    local seq n=0
    for seq in $(jq -r '.entries[].dlq_sequence' <<<"$BODY"); do
        sys POST "/sysadmin/dlq/$seq/ack"; expect 200 "acknowledge $seq"; n=$((n + 1))
    done
    ok "$n dead letters acknowledged; $(dashboard_line)"
}

gdpr() {
    step "6. Bob deletes his account (GDPR)"
    call "$SHOP_HOST" "$BOB" DELETE /me
    expect 409 "delete with an open order"
    info "refused while $BOB_ORDER is open: $(jq -r .detail <<<"$BODY")"
    shop POST "/admin/orders/$BOB_ORDER/package"
    expect 200 "package"
    local shipment; shipment=$(jq -r .shipment.shipment_id <<<"$BODY")
    order_is() { shop GET "/admin/orders/$BOB_ORDER"; [ "$(jq -r .status <<<"$BODY")" = "$1" ]; }
    shop POST "/admin/shipments/$shipment/manual-update" '{"status":"in_transit","carrier_name":"The Courier Guy","carrier_reference":"TCG-'"$RUN"'"}'
    expect 200 "in transit"
    poll 30 order_is shipped || fail "the order was not shipped"
    shop POST "/admin/shipments/$shipment/manual-update" '{"status":"delivered"}'
    expect 200 "delivered"
    poll 30 order_is delivered || fail "the order was not delivered"
    call "$SHOP_HOST" "$BOB" DELETE /me
    expect 204 "delete"
    ok "delivered, then DELETE /me → identity.user.deleted.v1"

    entry() { sys GET "/sysadmin/gdpr/register/$BOB_ID"; [ "$(jq -r .status <<<"$BODY")" = complete ]; }
    poll 90 entry || { echo "$BODY" | jq '{status, missing_services}'; fail "the GDPR deletion did not complete"; }
    local pseudonym; pseudonym=$(jq -r .pseudonym <<<"$BODY")
    jq -r '.reports[] | "    \(.service)\t\(.action)\t\(.record_count)"' <<<"$BODY" | column -t -s $'\t'
    info "complete at $(jq -r .completed_at <<<"$BODY") (deadline $(jq -r .deadline <<<"$BODY")); pseudonym $pseudonym"
    ok "every expected service reported platform.gdpr.cascade_completed.v1 to yc-audit's register"

    shop GET "/admin/orders/$BOB_ORDER"
    [ "$(jq -r .user_id <<<"$BODY")" = "$pseudonym" ] || fail "the order is not pseudonymised"
    local facts
    facts=$(docker exec yc-postgres psql -U postgres -d yc_analytics -Atc \
        "SELECT count(*) FROM fact_order WHERE user_id = '$pseudonym'")
    [ "$facts" -ge 1 ] || fail "analytics facts are not pseudonymised"
    [ "$(docker exec yc-postgres psql -U postgres -d yc_analytics -Atc "SELECT count(*) FROM fact_order WHERE user_id = '$BOB_ID'")" = 0 ] ||
        fail "analytics still holds Bob's ID"
    ok "the order and $facts analytics fact(s) carry the pseudonym; Bob's ID is gone"
}

exit_criteria() {
    step "7. Exit criteria"
    local out
    out=$(docker exec -e PGPASSWORD=devpass yc-postgres psql -h localhost -U yc_audit_app -d yc_audit -Atc \
        "SELECT has_table_privilege('audit_records','UPDATE'), has_table_privilege('audit_records','DELETE'),
                has_table_privilege('audit_records','TRUNCATE'), has_table_privilege('audit_records','INSERT')")
    [ "$out" = "f|f|f|t" ] || fail "yc_audit_app privileges on audit_records: $out"
    if docker exec -e PGPASSWORD=devpass yc-postgres psql -h localhost -U yc_audit_app -d yc_audit -Atc \
        "UPDATE audit_records SET summary = 'tampered'" >"$TMP/psql" 2>&1; then
        fail "yc_audit_app could update audit records"
    fi
    info "UPDATE as yc_audit_app: $(head -1 "$TMP/psql")"
    ok "audit records are insert-only for the service's own role (UPDATE, DELETE, TRUNCATE revoked)"

    consistent() {
        sys GET "/sysadmin/analytics/consistency?from=$(date -u -v-7d +%F 2>/dev/null || date -u -d '7 days ago' +%F)"
        [ "$STATUS" = 200 ] && jq -e '.consistent and .pending_days == 0' <<<"$BODY"
    }
    poll 30 consistent || { echo "$BODY" | jq '{mismatches, pending_days}'; fail "aggregates differ from their facts"; }
    info "$(jq -r '"checked \(.range.from) to \(.range.to): \(.mismatches | length) mismatches, \(.pending_days) days pending"' <<<"$BODY")"
    ok "every aggregate matches what the facts compute"
}

healthy
short_intervals
monitored
forced_failure
provision
analytics
chargeback
dlq
gdpr
exit_criteria
printf '\n\033[32m✓ Phase 8 milestone complete.\033[0m\n'
info "Restore the default intervals: docker compose --profile app up -d"
