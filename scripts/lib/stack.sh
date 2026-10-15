# Shared by scripts/load-test.sh and scripts/chaos-test.sh: checks the
# running stack, points it at the fake Stripe, provisions shops with a
# catalog, stock, Stripe and shipping, and runs k6 against them. Source it
# from the repository root after `set -euo pipefail`.
#
# PREFIX names the shops (<PREFIX>-<RUN>-<name>); results land in
# test/load/results/<RUN>/.

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}
FAKE_STRIPE_PORT=${FAKE_STRIPE_PORT:-12111}
K6_IMAGE=${K6_IMAGE:-grafana/k6:2.3.0}
PRODUCTS=${PRODUCTS:-12}
PREFIX=${PREFIX:-load}
RUN=${RUN:-$(date +%s)}
RESULTS=$PWD/test/load/results/$RUN
export NATS_URL=${NATS_URL:-nats://localhost:4222}

TMP=$(mktemp -d)
FAKE_PID=
cleanup() {
    [ -z "$FAKE_PID" ] || kill "$FAKE_PID" 2>/dev/null || true
    rm -rf "$TMP"
}
trap cleanup EXIT
# A sleeping Mac freezes the stack but not the clock, so timed waits
# would expire with nothing having run.
if command -v caffeinate >/dev/null; then
    caffeinate -i -w $$ &
fi
mkdir -p "$RESULTS"
chmod 777 "$RESULTS" # k6 writes its summaries as another user

step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }

poll() {
    local deadline=$((SECONDS + $1)); shift
    until "$@" >/dev/null 2>&1; do
        [ $SECONDS -lt $deadline ] || return 1
        sleep 0.5
    done
}

# call <host> <auth> <method> <path> [json] sets STATUS and BODY; auth is a
# bearer token, "jar:<file>" or "-". Setting up a catalog outruns the shop
# admin's rate limit, so a 429 waits for its Retry-After and tries again.
call() {
    local host=$1 auth=$2 method=$3 path=$4 body=${5:-} wait
    local args=(-sS -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' -X "$method" -H "Host: $host")
    case $auth in
        -) ;;
        jar:*) args+=(-b "${auth#jar:}" -c "${auth#jar:}") ;;
        *) args+=(-H "Authorization: Bearer $auth") ;;
    esac
    [ -z "$body" ] || args+=(-H "Content-Type: application/json" --data "$body")
    while :; do
        STATUS=$(curl "${args[@]}" "$GATEWAY$path")
        BODY=$(cat "$TMP/body")
        [ "$STATUS" = 429 ] || return 0
        wait=$(tr -d '\r' <"$TMP/headers" | sed -n 's/^[Rr]etry-[Aa]fter: //p')
        sleep "${wait:-1}"
    done
}

expect() {
    local what=${*: -1} want
    for want in "${@:1:$#-1}"; do
        [ "$STATUS" = "$want" ] && return 0
    done
    echo "$BODY" | head -c 800 | sed 's/^/    /'; echo
    fail "$what: HTTP $STATUS, want ${*:1:$#-1}"
}

readyz() { curl -fsS "http://localhost:$1/readyz" >/dev/null; }
routed() { ! curl -s -H "Host: $1" "$GATEWAY$2" | grep -q 'Route not found'; }
env_of() { docker inspect "$1" -f '{{range .Config.Env}}{{println .}}{{end}}' | sed -n "s/^$2=//p"; }

# Host ports of the gateway and every service on the checkout path.
CHECKOUT_PORTS="8090 8081 8083 8084 8087 8088 8089 8092 8093 8094 8098 8082"

healthy() {
    step "Services are up"
    for port in $CHECKOUT_PORTS; do
        readyz "$port" || fail "localhost:$port/readyz (task devstack:app)"
    done
    ok "gateway and every service on the checkout path are ready"
}

stack_config() {
    step "Gateway trusts the load generator; payments use the fake Stripe"
    NETWORK=$(docker inspect yc-gateway -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}')
    local subnet; subnet=$(docker network inspect "$NETWORK" -f '{{(index .IPAM.Config 0).Subnet}}')
    if [ "$(env_of yc-gateway YC_GATEWAY__TRUSTED_PROXIES)" != "$subnet" ]; then
        YC_GATEWAY_TRUSTED_PROXIES=$subnet docker compose --profile app up -d --no-build yc-gateway 2>&1 | sed 's/^/    /'
        poll 60 readyz 8090 || fail "yc-gateway did not come back"
    fi
    ok "yc-gateway trusts $subnet ($NETWORK) for X-Forwarded-For"

    STRIPE_API_KEY=sk_test_fake_load
    go build -o "$TMP/fake-stripe" ./services/yc-payment/cmd/fake-stripe
    "$TMP/fake-stripe" -addr ":$FAKE_STRIPE_PORT" >"$TMP/fake-stripe.log" 2>&1 &
    FAKE_PID=$!
    poll 10 curl -s "http://localhost:$FAKE_STRIPE_PORT/v1/balance" || fail "fake Stripe did not start"
    local base=http://host.docker.internal:$FAKE_STRIPE_PORT
    if [ "$(env_of yc-payment YC_PAYMENT_STRIPE_API_BASE)" != "$base" ]; then
        YC_PAYMENT_STRIPE_API_BASE=$base docker compose --profile app up -d --no-build yc-payment 2>&1 | sed 's/^/    /'
        poll 60 readyz 8089 || fail "yc-payment did not come back"
    fi
    ok "yc-payment uses the fake Stripe on localhost:$FAKE_STRIPE_PORT"
}

sysadmin() {
    docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
        /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
    call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
    expect 200 "system admin login"
    SYS=$(jq -r .access_token <<<"$BODY")
}

WORDS=(kettle mug lamp chair shirt book)

# shop <name>: provisions <PREFIX>-<RUN>-<name> with PRODUCTS products, 100 000
# of each in stock, Stripe and a zone, and appends it to $TMP/shops.
shop() {
    local tenant=$PREFIX-$RUN-$1 host=$PREFIX-$RUN-$1.localhost
    step "Shop $tenant"
    go run ./tools/yc-tenant-cli provision --tenant-id "$tenant" --name "Load Shop $1" --hostname "$host" \
        --store-currency ZAR --default-jurisdiction ZA --tax-inclusive=true >/dev/null
    poll 30 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $host' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $host"
    for path in /products /checkout /admin/zones /admin/gateway-config /admin/stock; do
        poll 45 routed "$host" "$path" || fail "no gateway route for $path"
    done

    local jar=$TMP/owner-$1.jar email="owner+$RUN-$1@load.test" password="owner-password-$RUN" user_id admin
    call "$host" "jar:$jar" GET /me
    call "$host" "jar:$jar" POST /register "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 201 "register owner"
    user_id=$(jq -r .user_id <<<"$BODY")
    call "$ADMIN_HOST" "$SYS" POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote"
    call "$host" - POST /login "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 200 "shop admin login"
    admin=$(jq -r .access_token <<<"$BODY")

    call "$host" "$admin" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 200 "pricing config"
    call "$host" "$admin" PUT /admin/tax-classes/za-standard \
        '{"display_name":"Standard VAT","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0.15","base":"line_total","sort_order":0}]}'
    expect 200 201 "tax class"
    call "$host" "$admin" GET /admin/inventory/config
    expect 200 "inventory config"
    call "$host" "$admin" PUT /admin/inventory/config "$(jq -c '{inventory_enabled: true,
        default_reservation_ttl_minutes, min_reservation_ttl_minutes, max_reservation_ttl_minutes}' <<<"$BODY")"
    expect 200 "turn inventory on"

    local i word id sku products=() skus=()
    for i in $(seq 1 "$PRODUCTS"); do
        word=${WORDS[$((i % ${#WORDS[@]}))]}
        call "$host" "$admin" POST /admin/products "{\"name\":\"$(tr '[:lower:]' '[:upper:]' <<<"${word:0:1}")${word:1} $i\",\"description\":\"A $word for the load test\",\"is_digital\":false}"
        expect 201 "product $i"
        id=$(jq -r .product_id <<<"$BODY")
        call "$host" "$admin" POST "/admin/products/$id/skus" \
            "{\"sku_code\":\"LOAD-$RUN-$1-$i\",\"tax_class\":\"za-standard\",\"inventory_tracked\":true,\"weight_grams\":1000}"
        expect 201 "SKU $i"
        sku=$(jq -r .sku_id <<<"$BODY")
        call "$host" "$admin" PUT "/admin/prices/sku/$sku" "{\"price\":{\"amount_minor\":$((5000 + i * 1000)),\"currency\":\"ZAR\"}}"
        expect 200 201 "price $i"
        products+=("$id") skus+=("$sku")
    done
    for sku in "${skus[@]}"; do
        has_stock() { call "$host" "$admin" GET "/admin/stock/$sku"; [ "$STATUS" = 200 ]; }
        poll 30 has_stock || fail "yc-inventory has no stock row for $sku"
        call "$host" "$admin" POST "/admin/stock/$sku/receive" '{"quantity":100000,"reason":"load test"}'
        expect 200 "receive stock"
    done
    ok "$PRODUCTS products, 100 000 of each in stock"

    call "$host" "$admin" PUT /admin/gateway-config/stripe "{\"enabled\":true,\"api_key\":\"$STRIPE_API_KEY\"}"
    expect 200 "Stripe credentials"
    call "$host" "$admin" POST /admin/zones '{"name":"South Africa","priority":10,"match_rules":[{"country_code":"ZA"}]}'
    expect 201 "zone"
    call "$host" "$admin" POST /admin/rates "{\"zone_id\":\"$(jq -r .zone_id <<<"$BODY")\",\"method\":\"standard\",\"display_name\":\"Standard\",\"pricing_type\":\"flat\",\"flat_amount_minor\":6000,\"estimated_days_min\":2,\"estimated_days_max\":3}"
    expect 201 "rate"
    ok "Stripe on; flat R60 shipping in South Africa"

    jq -n --arg name "$1" --arg host "$host" --arg tenant "$tenant" \
        '{name: $name, host: $host, tenant: $tenant, products: $ARGS.positional[0:'"$PRODUCTS"'], skus: $ARGS.positional['"$PRODUCTS"':]}' \
        --args "${products[@]}" "${skus[@]}" >>"$TMP/shops"
}

# catalog writes the shops provisioned so far for k6.
catalog() {
    jq -s '{shops: .}' "$TMP/shops" >"$RESULTS/catalog.json"
    jq '.shops' "$RESULTS/catalog.json" >"$RESULTS/shops.json"
}

k6() { # k6 <script> [-e K=V...] [k6 run flags...]
    local script=$1; shift
    local envs=() flags=()
    while [ $# -gt 0 ]; do
        case $1 in
            -e) envs+=(-e "$2"); shift 2 ;;
            *) flags+=("$1"); shift ;;
        esac
    done
    docker run --rm --network "$NETWORK" -v "$PWD/test/load/k6:/scripts:ro" -v "$RESULTS:/results" \
        ${envs[@]+"${envs[@]}"} "$K6_IMAGE" run --quiet ${flags[@]+"${flags[@]}"} "/scripts/$script"
}
