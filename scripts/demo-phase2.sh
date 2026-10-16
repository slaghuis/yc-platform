#!/usr/bin/env bash
# Phase 2 milestone demo and exit criteria (docs/implementation-plan.md,
# "Phase 2 — Catalog and Browse").
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase2.sh
#
# Everything goes through yc-gateway: admin.localhost is the platform host
# (system admins), $SHOP_HOST is the shop. Needs curl, jq, docker and go;
# the USD step needs yc-pricing to reach api.frankfurter.dev. Safe to re-run:
# each run uses fresh emails, products and SKU codes.
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

healthy() {
    step "0. Services are up and registered with the gateway"
    for port in 8090 8081 8082 8083 8084 8085; do
        curl -fsS "http://localhost:$port/readyz" >/dev/null || fail "localhost:$port/readyz"
    done
    ok "gateway, identity, monitor, product, pricing and image are ready"
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
    ok "yc-gateway resolves $SHOP_HOST → $TENANT"
    for path in /products /prices/sku/00000000-0000-0000-0000-000000000000 /images/00000000-0000-0000-0000-000000000000; do
        poll 45 routed "$path" || fail "no gateway route for $path"
    done
    ok "product, pricing and image routes registered with the gateway (ADR-020 rule 7)"
}

system_admin() {
    step "1. A system admin seeds a supplier"
    docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
        /app/service bootstrap-admin --email "$SYSADMIN_EMAIL" | sed 's/^/    /'
    call "$ADMIN_HOST" - POST /login "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}"
    expect 200 "system admin login on $ADMIN_HOST"
    SYS=$(jq -r .access_token <<<"$BODY")
    ok "logged in on $ADMIN_HOST as $(jq -r '"\(.role) in \(.tenant_id)"' <<<"$BODY")"

    call "$ADMIN_HOST" "$SYS" POST /sysadmin/suppliers "{\"tenant_id\":\"$TENANT\",\"name\":\"Acme Textiles $RUN\"}"
    expect 201 "create supplier"
    SUPPLIER=$(jq -r .supplier_id <<<"$BODY")
    ok "supplier Acme Textiles $RUN ($SUPPLIER) for $TENANT"
}

shop_admin() {
    step "Shop admin: register as a customer, promoted by the system admin"
    local jar=$TMP/owner.jar email="owner+$RUN@demo-shop.test" password="owner-password-$RUN"
    call "$SHOP_HOST" "jar:$jar" GET /me
    expect 200 "visitor"
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"$email\",\"password\":\"$password\",\"display_name\":\"Shop Owner\"}"
    expect 201 "register"
    local user_id customer
    user_id=$(jq -r .user_id <<<"$BODY")
    customer=$(jq -r .access_token <<<"$BODY")
    ok "registered $email as $(jq -r .role <<<"$BODY") $user_id"

    call "$SHOP_HOST" "$customer" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 403 "customer on an admin route"
    ok "a customer gets 403 on /admin/pricing-config"

    call "$ADMIN_HOST" "$SYS" POST "/admin/users/$user_id/promote" '{"role":"shop_admin"}'
    expect 200 "promote"
    call "$SHOP_HOST" - POST /login "{\"email\":\"$email\",\"password\":\"$password\"}"
    expect 200 "shop admin login"
    ADMIN=$(jq -r .access_token <<<"$BODY")
    ok "promoted; logged in on $SHOP_HOST as $(jq -r .role <<<"$BODY")"
}

catalog() {
    step "2. The shop admin creates products, variants and SKUs"
    call "$SHOP_HOST" "$ADMIN" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 200 "pricing config"
    local vat='{"display_name":"Standard VAT","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0.15","base":"line_total","sort_order":0}]}'
    call "$SHOP_HOST" "$ADMIN" PUT /admin/tax-classes/za-standard "$vat"
    expect 200 201 "tax class za-standard"
    call "$SHOP_HOST" "$ADMIN" PUT /admin/tax-classes/za-zero \
        '{"display_name":"Zero-rated","rules":[{"jurisdiction":"ZA","rule_name":"VAT","rate":"0","base":"line_total","sort_order":0}]}'
    expect 200 201 "tax class za-zero"
    ok "tax classes za-standard (VAT 15%) and za-zero (VAT 0%)"

    call "$SHOP_HOST" "$ADMIN" POST /admin/products \
        "{\"name\":\"Organic T-Shirt $RUN\",\"description\":\"Soft cotton crew neck\",\"supplier_id\":\"$SUPPLIER\"}"
    expect 201 "create shirt"
    SHIRT=$(jq -r .product_id <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$SHIRT/variants" '{"axis_name":"colour","values":["Red","Blue"]}'
    expect 201 "variant axis"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$SHIRT/skus" \
        "{\"sku_code\":\"TS-RED-$RUN\",\"variant_values\":{\"colour\":\"Red\"},\"tax_class\":\"za-standard\"}"
    expect 201 "SKU red"
    RED=$(jq -r .sku_id <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$SHIRT/skus" \
        "{\"sku_code\":\"TS-BLUE-$RUN\",\"variant_values\":{\"colour\":\"Blue\"},\"tax_class\":\"za-standard\"}"
    expect 201 "SKU blue"
    BLUE=$(jq -r .sku_id <<<"$BODY")
    ok "Organic T-Shirt $RUN: colour Red/Blue, SKUs TS-RED-$RUN, TS-BLUE-$RUN"

    call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"Field Guide $RUN\",\"description\":\"Birds of the Cape\"}"
    expect 201 "create book"
    BOOK_PRODUCT=$(jq -r .product_id <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$BOOK_PRODUCT/skus" "{\"sku_code\":\"BOOK-$RUN\",\"tax_class\":\"za-zero\"}"
    expect 201 "SKU book"
    BOOK=$(jq -r .sku_id <<<"$BODY")

    call "$SHOP_HOST" "$ADMIN" POST /admin/products "{\"name\":\"Enamel Mug $RUN\",\"description\":\"Priced excluding VAT\"}"
    expect 201 "create mug"
    MUG_PRODUCT=$(jq -r .product_id <<<"$BODY")
    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$MUG_PRODUCT/skus" "{\"sku_code\":\"MUG-$RUN\",\"tax_class\":\"za-standard\"}"
    expect 201 "SKU mug"
    MUG=$(jq -r .sku_id <<<"$BODY")
    ok "Field Guide $RUN (zero-rated) and Enamel Mug $RUN"
}

prices() {
    step "3. The shop admin sets base prices"
    price() {
        call "$SHOP_HOST" "$ADMIN" PUT "/admin/prices/sku/$1" "{\"price\":{\"amount_minor\":$2,\"currency\":\"ZAR\"}}"
        expect 200 201 "price $1"
    }
    price "$RED" 20000
    price "$BLUE" 20000
    price "$BOOK" 15000
    ok "T-Shirt R200.00 and Field Guide R150.00, VAT-inclusive (the shop's setting)"

    call "$SHOP_HOST" "$ADMIN" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":false,"default_jurisdiction":"ZA"}'
    expect 200 "switch to tax-exclusive"
    price "$MUG" 10000
    call "$SHOP_HOST" "$ADMIN" PUT /admin/pricing-config '{"store_currency":"ZAR","tax_inclusive":true,"default_jurisdiction":"ZA"}'
    expect 200 "switch back to tax-inclusive"
    ok "Enamel Mug R100.00 set while the shop was tax-exclusive (each price keeps its own meaning)"
}

images() {
    step "4. The shop admin uploads an image"
    mkdir -p "$TMP/genpng"
    cat >"$TMP/genpng/main.go" <<'GO'
package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	img := image.NewNRGBA(image.Rect(0, 0, 1600, 1200))
	for y := range 1200 {
		for x := range 1600 {
			c := color.NRGBA{R: 200, G: 40, B: 50, A: 255}
			if (x/100+y/100)%2 == 0 {
				c = color.NRGBA{R: uint8(x / 7), G: uint8(y / 5), B: 160, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	_ = png.Encode(os.Stdout, img)
}
GO
    (cd "$TMP/genpng" && GOWORK=off GOFLAGS= go run main.go) >"$TMP/shirt.png"
    info "generated a 1600×1200 PNG ($(wc -c <"$TMP/shirt.png" | tr -d ' ') bytes)"

    call "$SHOP_HOST" "$ADMIN" POST /admin/images "" -F "file=@$TMP/shirt.png" -F "owner_ref=product:$SHIRT"
    expect 202 "upload"
    IMAGE=$(jq -r .image_id <<<"$BODY")
    ok "accepted $IMAGE (status $(jq -r .status <<<"$BODY"))"

    image_ready() {
        call "$SHOP_HOST" "$ADMIN" GET "/images/$IMAGE"
        [ "$(jq -r .status <<<"$BODY")" = ready ]
    }
    poll 30 image_ready || { call "$SHOP_HOST" "$ADMIN" GET "/images/$IMAGE"; echo "$BODY"; fail "image not ready"; }
    jq -r '.variants[] | "    \(.variant) \(.format) \(.width)×\(.height) \(.bytes) bytes"' <<<"$BODY"
    local thumb
    thumb=$(jq -r '.variants[] | select(.variant == "thumbnail" and .format == "webp") | .url' <<<"$BODY")
    [ "$(curl -s -o /dev/null -w '%{http_code} %{content_type}' "$thumb")" = "200 image/webp" ] || fail "fetch $thumb"
    ok "ready; thumbnail served from $thumb"

    call "$SHOP_HOST" "$ADMIN" POST "/admin/products/$SHIRT/images" "{\"image_id\":\"$IMAGE\",\"is_primary\":true}"
    expect 201 "attach image"
    ok "attached as the T-Shirt's primary image"

    call "$SHOP_HOST" "$ADMIN" POST /admin/images/upload-url \
        "{\"content_type\":\"image/png\",\"bytes\":$(wc -c <"$TMP/shirt.png" | tr -d ' '),\"owner_ref\":\"product:$SHIRT\"}"
    expect 201 "presigned upload URL"
    local upload_url
    upload_url=$(jq -r .upload_url <<<"$BODY")
    IMAGE=$(jq -r .image.image_id <<<"$BODY")
    curl -fsS -X PUT -H "Content-Type: image/png" --data-binary "@$TMP/shirt.png" "$upload_url" ||
        fail "PUT to the presigned URL"
    call "$SHOP_HOST" "$ADMIN" POST "/admin/images/$IMAGE/finalize"
    expect 202 "finalise"
    poll 30 image_ready || fail "presigned image not ready"
    ok "presigned upload straight to S3, finalised and processed ($IMAGE)"
}

browse() {
    step "5. A visitor browses GET /products with prices in ZAR"
    local jar=$TMP/visitor.jar
    call "$SHOP_HOST" "jar:$jar" GET /me
    expect 200 "visitor"
    VISITOR="jar:$jar"

    listed() {
        call "$SHOP_HOST" "$VISITOR" GET "/products?q=$RUN&page_size=50"
        [ "$STATUS" = 200 ] && [ "$(jq '.items | length' <<<"$BODY")" = 3 ]
    }
    poll 15 listed || { echo "$BODY"; fail "the three products are not browsable"; }
    cp "$TMP/body" "$TMP/products.json"
    local lines
    lines=$(jq -c '{lines: [.items[].skus[] | {sku_id, quantity: 1}]}' "$TMP/products.json")

    priced() {
        call "$SHOP_HOST" "$VISITOR" POST /prices/evaluate "$lines"
        [ "$STATUS" = 200 ] && [ "$(jq '.unpriced | length' <<<"$BODY")" = 0 ]
    }
    poll 15 priced || { echo "$BODY"; fail "not every SKU is priced"; }
    cp "$TMP/body" "$TMP/zar.json"
    jq -r --slurpfile p "$TMP/products.json" "$JQ_MONEY"'
        ($p[0].items | map(.name as $n | .skus[] | {key: .sku_id, value: "\($n) \(.sku_code)"}) | from_entries) as $names |
        .lines[] | "    \($names[.sku_id])  \(.gross | amt)  (net \(.net | amt), VAT \(.tax | amt))"' "$TMP/zar.json"
    ok "catalog from yc-product, prices from yc-pricing (POST /prices/evaluate)"

    step "Exit criteria: tax-inclusive, tax-exclusive and the VAT breakdown"
    line() { jq -c --arg s "$1" '.lines[] | select(.sku_id == $s) | [.net.amount_minor, .tax.amount_minor, .gross.amount_minor]' "$TMP/zar.json"; }
    [ "$(line "$RED")" = "[17391,2609,20000]" ] || fail "T-Shirt: $(line "$RED"), want net 17391, VAT 2609, gross 20000"
    ok "inclusive R200.00 at 15%: net R173.91 + VAT R26.09 = R200.00"
    [ "$(line "$MUG")" = "[10000,1500,11500]" ] || fail "Mug: $(line "$MUG"), want net 10000, VAT 1500, gross 11500"
    ok "exclusive R100.00 at 15%: net R100.00 + VAT R15.00 = R115.00"
    [ "$(line "$BOOK")" = "[15000,0,15000]" ] || fail "Field Guide: $(line "$BOOK")"
    ok "zero-rated R150.00: no VAT"
    jq -e '.totals | (.subtotal_excl_tax.amount_minor + .tax_total.amount_minor == .grand_total.amount_minor)
        and ([.tax_breakdown[].amount.amount_minor] | add) == .tax_total.amount_minor' "$TMP/zar.json" >/dev/null ||
        fail "totals do not add up"
    jq -r "$JQ_MONEY"' .totals | "    subtotal \(.subtotal_excl_tax | amt) + VAT \(.tax_total | amt) = \(.grand_total | amt)",
        (.tax_breakdown[] | "      \(.rule) @ \(.rate): \(.amount | amt)")' "$TMP/zar.json"
    ok "breakdown by rule and rate sums to the VAT total"
}

usd() {
    step "6. With a display currency the visitor sees USD"
    local lines
    lines=$(jq -c '{lines: [.lines[] | {sku_id, quantity}]}' "$TMP/zar.json")
    call "$SHOP_HOST" "$VISITOR" POST /prices/evaluate "$lines" -H "X-YC-Display-Currency: USD"
    if [ "$STATUS" = 400 ] && grep -q unavailable <<<"$BODY"; then
        fail "no ZAR→USD rate yet: yc-pricing fetches from api.frankfurter.dev at startup (docker logs yc-pricing)"
    fi
    expect 200 "evaluate in USD"
    cp "$TMP/body" "$TMP/usd.json"
    jq -r "$JQ_MONEY"' .display | "    1 ZAR = \(.rate) USD (\(.provider), fetched \(.rate_fetched_at))",
        (.lines[] | "    \(.sku_id[0:8])…  \(.gross | amt)"), "    total \(.totals.grand_total | amt)"' "$TMP/usd.json"
    jq -e '.display as $d | ($d.rate | tonumber) as $r | [.lines[] as $l | ($d.lines[] | select(.sku_id == $l.sku_id)) as $u |
        (($l.gross.amount_minor * $r) | round) == $u.gross.amount_minor] | all' "$TMP/usd.json" >/dev/null ||
        fail "USD amounts are not gross × rate"
    jq -e '.store_currency == "ZAR" and .totals.grand_total.currency == "ZAR"' "$TMP/usd.json" >/dev/null ||
        fail "store-currency amounts changed"
    ok "each USD amount is the ZAR amount × rate, rounded; ZAR amounts unchanged"

    call "$SHOP_HOST" "$VISITOR" GET "/prices/sku/$RED?display_currency=USD"
    expect 200 "single SKU in USD"
    ok "GET /prices/sku/{id}?display_currency=USD → $(jq -r "$JQ_MONEY"' .display.lines[0].gross | amt' <<<"$BODY")"
}

audit() {
    step "7. A single product shows its pricing audit trail"
    call "$SHOP_HOST" "$VISITOR" GET "/products/$SHIRT"
    expect 200 "product"
    info "$(jq -r '"\(.name): \(.skus | length) SKUs, \(.images | length) image(s)"' <<<"$BODY")"
    local lines
    lines=$(jq -c '{lines: [.skus[] | {sku_id, quantity: 1}], display_currency: "USD"}' <<<"$BODY")
    call "$SHOP_HOST" "$VISITOR" POST /prices/evaluate "$lines"
    expect 200 "evaluate product"
    jq -r '.audit_trail[] | "    \(.evaluator): \(.detail | tojson | .[0:150])"' <<<"$BODY"
    [ "$(jq -r '[.audit_trail[].evaluator] | join(",")' <<<"$BODY")" = "base_price,tax,display_currency" ] ||
        fail "audit trail is not base_price → tax → display_currency"
    ok "base_price → tax → display_currency"
}

roles() {
    step "Exit criteria: admin APIs are protected by role"
    check() { # check <status...> <description> -- <call args...>
        local want=() desc
        while [ "$1" != -- ]; do want+=("$1"); shift; done
        desc=${want[*]: -1}; unset 'want[${#want[@]}-1]'; shift
        call "$@"
        expect "${want[@]}" "$desc"
        ok "$desc → $STATUS"
    }
    check 401 "visitor creates a product (sign in required)" -- "$SHOP_HOST" "$VISITOR" POST /admin/products '{"name":"x"}'
    check 401 "visitor sets a price" -- "$SHOP_HOST" "$VISITOR" PUT "/admin/prices/sku/$RED" '{"price":{"amount_minor":1,"currency":"ZAR"}}'
    check 401 "visitor uploads an image" -- "$SHOP_HOST" "$VISITOR" POST /admin/images ""
    local jar=$TMP/shopper.jar
    call "$SHOP_HOST" "jar:$jar" GET /me
    call "$SHOP_HOST" "jar:$jar" POST /register "{\"email\":\"shopper+$RUN@demo-shop.test\",\"password\":\"shopper-password\"}"
    expect 201 "register a shopper"
    check 403 "customer reads the pricing config" -- "$SHOP_HOST" "$(jq -r .access_token <<<"$BODY")" GET /admin/pricing-config ""
    check 403 "shop admin creates a supplier" -- "$SHOP_HOST" "$ADMIN" POST /sysadmin/suppliers "{\"tenant_id\":\"$TENANT\",\"name\":\"x\"}"
    check 401 "no token on the admin host" -- "$ADMIN_HOST" - GET /admin/users ""
    check 401 403 "system admin token on the shop host" -- "$SHOP_HOST" "$SYS" GET /admin/pricing-config ""
}

healthy
provision
system_admin
shop_admin
catalog
prices
images
browse
usd
audit
roles
printf '\n\033[32m✓ Phase 2 milestone complete.\033[0m\n'
