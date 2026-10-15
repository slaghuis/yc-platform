#!/usr/bin/env bash
# Phase 9 load test (docs/implementation-plan.md, "Phase 9 — Hardening";
# results in docs/reports/load-test.md).
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/load-test.sh [browse] [checkout] [events]   # default: all three
#
#   browse    RATE (500) requests/s on /products for DURATION (60s), spread
#             over three new shops, each virtual user a visitor with its own
#             address
#   checkout  CHECKOUT_VUS (10) shoppers checking out back to back for
#             DURATION, paying through the fake Stripe
#   events    EVENT_RATE (1000) identity.visitor.created.v1 a second through
#             yc-eventbus's outbox for DURATION (go run ./test/load/eventload)
#
# k6 runs from the grafana/k6 image on the stack's network. yc-gateway is
# restarted to trust that network as a proxy (YC_GATEWAY_TRUSTED_PROXIES),
# so each virtual user's X-Forwarded-For is its address, as behind a load
# balancer; yc-payment is restarted against the fake Stripe. Rate limits stay
# on. Results land in test/load/results/<run>/. Needs curl, jq, docker and go.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/stack.sh
source scripts/lib/stack.sh

RATE=${RATE:-500}
CHECKOUT_VUS=${CHECKOUT_VUS:-10}
EVENT_RATE=${EVENT_RATE:-1000}
DURATION=${DURATION:-60s}

SCENARIOS=("$@")
[ ${#SCENARIOS[@]} -gt 0 ] || SCENARIOS=(browse checkout events)
wants() { [[ " ${SCENARIOS[*]} " == *" $1 "* ]]; }

browse() {
    step "Browse: $RATE requests/s on /products for $DURATION"
    k6 browse.js -e RATE="$RATE" -e DURATION="$DURATION" || BROWSE_FAILED=1
}

checkout() {
    step "Checkout: $CHECKOUT_VUS concurrent shoppers for $DURATION"
    k6 checkout.js -e VUS="$CHECKOUT_VUS" -e DURATION="$DURATION" || CHECKOUT_FAILED=1
}

events() {
    step "Events: $EVENT_RATE identity.visitor.created.v1 a second for $DURATION"
    local pg; pg=$(docker compose ps -q postgres)
    docker exec "$pg" psql -U postgres -tc "SELECT 1 FROM pg_database WHERE datname = 'yc_load'" | grep -q 1 ||
        docker exec "$pg" psql -U postgres -qc "CREATE DATABASE yc_load"
    go run ./test/load/eventload -rate "$EVENT_RATE" -duration "$DURATION" \
        -tenants "$(jq -r 'map(.tenant) | join(",")' <"$RESULTS/shops.json")" \
        -out "$RESULTS/events.json" 2>"$RESULTS/events.log" || EVENTS_FAILED=1
}

healthy
stack_config
sysadmin
for n in 1 2 3; do shop "$n"; done
catalog
sleep 2 # let the search index and price projections settle

BROWSE_FAILED= CHECKOUT_FAILED= EVENTS_FAILED=
wants browse && browse
wants checkout && checkout
wants events && events

step "Results in ${RESULTS#"$PWD"/}"
[ -z "$BROWSE_FAILED$CHECKOUT_FAILED$EVENTS_FAILED" ] || fail "thresholds failed:${BROWSE_FAILED:+ browse}${CHECKOUT_FAILED:+ checkout}${EVENTS_FAILED:+ events}"
printf '\n\033[32m✓ Load test passed.\033[0m\n'
