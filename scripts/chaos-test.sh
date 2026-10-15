#!/usr/bin/env bash
# Phase 9 chaos test (docs/implementation-plan.md, "Phase 9 — Hardening";
# results in docs/reports/chaos-test.md).
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/chaos-test.sh [drill...]        # default: every drill
#
# Each drill gets its own shop. CHAOS_VUS (4) shoppers check out back to
# back (test/load/k6/checkout.js) while the drill breaks something and puts
# it back, and for RECOVERY (30s) afterwards. Then every saga must settle
# within SETTLE (300s) and the shop's books must agree:
#   - no checkout session is left mid-saga;
#   - each completed checkout has one order, received, and no other order
#     is live;
#   - each completed checkout's payment is captured, and no other payment is
#     captured or left open;
#   - each completed checkout's reservation is committed and no other is
#     held; the stock taken is exactly the units committed and nothing is
#     left reserved.
#
# Drills:
#   kill-<service>   SIGKILL the service, start it again after OUTAGE (20s):
#                    checkout cart pricing inventory payment orders shipping
#                    gateway identity
#   nats             SIGKILL NATS for OUTAGE; afterwards every outbox must
#                    drain and every consumer catch up
#   pause-<service>  freeze the service (docker pause) for PAUSE (45s), past
#                    its callers' timeouts (payment 35s, internal calls 10s):
#                    payment inventory postgres
#
# Results land in test/load/results/<run>/chaos.json. Needs curl, jq, docker
# and go. The drills restart containers of the local stack only.
set -euo pipefail
cd "$(dirname "$0")/.."
PREFIX=chaos
PRODUCTS=${PRODUCTS:-3}
# shellcheck source=lib/stack.sh
source scripts/lib/stack.sh

CHAOS_VUS=${CHAOS_VUS:-4}
OUTAGE=${OUTAGE:-20}
PAUSE=${PAUSE:-45}
RECOVERY=${RECOVERY:-30}
SETTLE=${SETTLE:-300}
LEAD=10 # seconds of normal traffic before the drill strikes

ALL=(kill-checkout kill-cart kill-pricing kill-inventory kill-payment kill-orders kill-shipping
    kill-gateway kill-identity nats pause-payment pause-inventory pause-postgres)
DRILLS=("$@")
[ ${#DRILLS[@]} -gt 0 ] || DRILLS=("${ALL[@]}")

PG=yc-postgres
q() { docker exec "$PG" psql -U postgres -d "$1" -Atc "$2"; }

# The container a drill has broken and how to put it back, so a script
# that stops early never leaves it down.
STRUCK= RESTORE=
trap '[ -z "$STRUCK" ] || $RESTORE "$STRUCK" >/dev/null 2>&1 || true; cleanup' EXIT
sorted() { LC_ALL=C sort; }

# back <container>: the container serves again.
back() {
    case $1 in
        yc-nats) curl -fsS http://localhost:8222/healthz ;;
        yc-postgres) docker exec "$PG" pg_isready -U postgres -q ;;
        *) readyz "$(docker port "$1" 8080/tcp | head -1 | sed 's/.*://')" ;;
    esac
}

# Databases with an outbox, and the rows still to relay.
OUTBOX_DBS=()
outbox_dbs() {
    local d
    for d in $(q postgres "SELECT datname FROM pg_database WHERE datname LIKE 'yc\_%' AND datname <> 'yc_load'"); do
        [ "$(q "$d" "SELECT to_regclass('outbox') IS NOT NULL")" = t ] && OUTBOX_DBS+=("$d")
    done
}
outbox_pending() { # outbox_pending [created before]
    local d n=0 cond="published_at IS NULL"
    [ -z "${1:-}" ] || cond="$cond AND created_at < '$1'"
    for d in "${OUTBOX_DBS[@]}"; do
        n=$((n + $(q "$d" "SELECT count(*) FROM outbox WHERE $cond")))
    done
    echo "$n"
}
drained() { [ "$(outbox_pending "$1")" = 0 ]; }

# Consumers not pulling before a drill (left over from removed
# subscriptions) are ignored when waiting for consumers to catch up.
jsz() { curl -fsS 'http://localhost:8222/jsz?consumers=true'; }
idle_consumers() {
    jsz | jq -c '[.account_details[].stream_detail[].consumer_detail[]? | select(.num_waiting == 0) | .name]' >"$TMP/idle.json"
}
backlog() {
    jsz | jq --slurpfile idle "$TMP/idle.json" \
        '[.account_details[].stream_detail[].consumer_detail[]? | select(.name as $n | $idle[0] | index($n) | not)
          | .num_pending + .num_ack_pending] | add // 0'
}
caught_up() { [ "$(backlog)" = 0 ]; }

FAILED=()
# verify <drill> <what> <got> <want>
verify() {
    if [ "$3" = "$4" ]; then
        ok "$2"
    else
        printf '  \033[31m✗ %s: got %s, want %s\033[0m\n' "$2" "$3" "$4"
        FAILED+=("$1: $2")
    fi
}
# same <drill> <what> <expected ids> <actual ids>
same() {
    local only_want only_got
    only_want=$(LC_ALL=C comm -23 <(sorted <<<"$3") <(sorted <<<"$4") | grep -c . || true)
    only_got=$(LC_ALL=C comm -13 <(sorted <<<"$3") <(sorted <<<"$4") | grep -c . || true)
    verify "$1" "$2 ($(grep -c . <<<"$4" || true))" "missing $only_want, extra $only_got" "missing 0, extra 0"
}

# client_failures <k6 log>: the shoppers' failures as {"<step>: HTTP <status>": n}.
client_failures() {
    { grep -oE 'GoError: [a-z ]+: (HTTP [0-9]+|state [A-Z_]+)' "$1" || true; } | sed 's/^GoError: //' | sort | uniq -c |
        jq -Rsc '[split("\n")[] | select(length > 0) | capture("^ *(?<n>[0-9]+) (?<what>.*)$") | {(.what): (.n | tonumber)}] | add // {}'
}

mid_saga() {
    q yc_checkout "SELECT count(*) FROM checkout_sessions WHERE tenant_id = '$1'
        AND state NOT IN ('PRICING', 'COMPLETED', 'FAILED', 'EXPIRED')"
}
settled() { [ "$(mid_saga "$1")" = 0 ]; }

reconcile() {
    local d=$1 t=$2 done_orders done_payments done_reservations
    done_orders=$(q yc_checkout "SELECT order_id FROM checkout_sessions WHERE tenant_id = '$t' AND state = 'COMPLETED'")
    done_payments=$(q yc_checkout "SELECT payment_transaction_id FROM checkout_sessions WHERE tenant_id = '$t' AND state = 'COMPLETED'")
    done_reservations=$(q yc_checkout "SELECT reservation_id FROM checkout_sessions WHERE tenant_id = '$t' AND state = 'COMPLETED'")

    verify "$d" "no checkout left mid-saga" "$(mid_saga "$t")" 0
    same "$d" "one live order per completed checkout" "$done_orders" \
        "$(q yc_orders "SELECT id FROM orders WHERE tenant_id = '$t' AND status NOT IN ('abandoned', 'cancelled')")"
    verify "$d" "every live order received" \
        "$(q yc_orders "SELECT count(*) FROM orders WHERE tenant_id = '$t' AND status NOT IN ('abandoned', 'cancelled', 'received')")" 0
    same "$d" "one captured payment per completed checkout" "$done_payments" \
        "$(q yc_payment "SELECT id FROM transactions WHERE tenant_id = '$t' AND status = 'captured'")"
    verify "$d" "no payment left open" \
        "$(q yc_payment "SELECT count(*) FROM transactions WHERE tenant_id = '$t' AND status NOT IN ('captured', 'voided', 'failed', 'refunded')")" 0
    same "$d" "one committed reservation per completed checkout" "$done_reservations" \
        "$(q yc_inventory "SELECT id FROM reservations WHERE tenant_id = '$t' AND status = 'committed'")"
    verify "$d" "no reservation left held" \
        "$(q yc_inventory "SELECT count(*) FROM reservations WHERE tenant_id = '$t' AND status NOT IN ('committed', 'released', 'expired')")" 0
    verify "$d" "stock taken = units committed, none reserved" \
        "$(q yc_inventory "SELECT (SELECT COALESCE(sum(100000 - on_hand), 0) FROM stock_levels WHERE tenant_id = '$t')
            - (SELECT COALESCE(sum(i.quantity), 0) FROM reservation_items i JOIN reservations r ON r.id = i.reservation_id
               WHERE r.tenant_id = '$t' AND r.status = 'committed')
            || '/' || (SELECT COALESCE(sum(reserved), 0) FROM stock_levels WHERE tenant_id = '$t')")" "0/0"
}

drill() {
    local name=$1 c strike restore secs extra='{}'
    case $name in
        kill-*) c=yc-${name#kill-} strike="docker kill" restore="docker start" secs=$OUTAGE ;;
        nats) c=yc-nats strike="docker kill" restore="docker start" secs=$OUTAGE ;;
        pause-*) c=yc-${name#pause-} strike="docker pause" restore="docker unpause" secs=$PAUSE ;;
        *) fail "unknown drill $name" ;;
    esac
    local tenant=$PREFIX-$RUN-$name duration=$((LEAD + secs + RECOVERY))
    step "$name: $strike $c for ${secs}s during checkouts"
    [ "$name" != nats ] || idle_consumers

    k6 checkout.js -e VUS="$CHAOS_VUS" -e DURATION="${duration}s" -e SHOP="$name" -e SUMMARY="$name" \
        --no-thresholds >"$RESULTS/$name.k6.log" 2>&1 &
    local k6_pid=$!
    sleep "$LEAD"
    STRUCK=$c RESTORE=$restore
    $strike "$c" >/dev/null
    local struck=$SECONDS peak=0 n
    if [ "$name" = nats ]; then
        while [ $((SECONDS - struck)) -lt "$secs" ]; do
            n=$(outbox_pending); [ "$n" -le "$peak" ] || peak=$n
            sleep 1
        done
    else
        sleep "$secs"
    fi
    # Outbox rows written before this point must drain (nats only: Postgres
    # may be the paused container).
    local restored_at=
    [ "$name" != nats ] || restored_at=$(q postgres "SELECT now()")
    $restore "$c" >/dev/null
    STRUCK=
    local restored=$SECONDS
    poll 120 back "$c" || fail "$c did not come back"
    local recovered=$((SECONDS - restored))
    info "$c back after ${recovered}s"

    if [ "$name" = nats ]; then
        local drained=-1
        poll 300 drained "$restored_at" && drained=$((SECONDS - restored))
        info "outbox peaked at $peak rows; the backlog from the outage relayed ${drained}s after NATS came back"
        extra=$(jq -nc --argjson peak "$peak" --argjson drained "$drained" '{outbox_peak: $peak, outbox_drained_s: $drained}')
    fi

    wait "$k6_pid" || true
    local stopped=$SECONDS settle=-1
    if poll "$SETTLE" settled "$tenant"; then
        settle=$((SECONDS - stopped))
        info "sagas settled ${settle}s after traffic stopped"
    else
        info "sagas not settled ${SETTLE}s after traffic stopped"
    fi
    if [ "$name" = nats ]; then
        local caught=-1
        poll 300 caught_up && caught=$((SECONDS - stopped))
        info "consumers caught up ${caught}s after traffic stopped (ignoring $(jq length "$TMP/idle.json") idle consumers)"
        extra=$(jq -c --argjson caught "$caught" '. + {consumers_caught_up_s: $caught}' <<<"$extra")
    fi

    local before=${#FAILED[@]}
    reconcile "$name" "$tenant"
    local client states
    client=$(jq -c --argjson failures "$(client_failures "$RESULTS/$name.k6.log")" \
        '{completed: (.metrics.checkouts_completed.values.count // 0),
          not_completed: (.metrics.checkouts_not_completed.values.count // 0), failures: $failures}' "$RESULTS/$name.json")
    states=$(q yc_checkout "SELECT COALESCE(json_object_agg(state, n), '{}') FROM
        (SELECT state, count(*) AS n FROM checkout_sessions WHERE tenant_id = '$tenant' GROUP BY 1) s")
    info "client: $client; sessions: $states"
    jq -nc --arg drill "$name" --arg tenant "$tenant" --argjson outage "$secs" --argjson recovered "$recovered" \
        --argjson settle "$settle" --argjson client "$client" --argjson sessions "$states" --argjson extra "$extra" \
        --argjson passed "$([ ${#FAILED[@]} -eq "$before" ] && echo true || echo false)" \
        '{drill: $drill, tenant: $tenant, outage_s: $outage, recovered_s: $recovered, settled_s: $settle,
          client: $client, sessions: $sessions, passed: $passed} + $extra' >>"$RESULTS/chaos.jsonl"
}

healthy
stack_config
sysadmin
for d in "${DRILLS[@]}"; do shop "$d"; done
catalog
outbox_dbs
sleep 2

for d in "${DRILLS[@]}"; do
    drill "$d"
    for port in $CHECKOUT_PORTS; do poll 120 readyz "$port" || fail "localhost:$port/readyz after $d"; done
done
jq -s . "$RESULTS/chaos.jsonl" >"$RESULTS/chaos.json"

step "Results in ${RESULTS#"$PWD"/}"
jq -r '.[] | [.drill, "client \(.client.completed)/\(.client.not_completed)",
    "sessions \(.sessions | to_entries | map("\(.key) \(.value)") | join(", "))",
    "back \(.recovered_s)s", "settled \(.settled_s)s", (if .passed then "ok" else "FAILED" end)] | join("  ")' \
    "$RESULTS/chaos.json" | sed 's/^/  /'
[ ${#FAILED[@]} -eq 0 ] || fail "$(printf '%s; ' "${FAILED[@]}")"
printf '\n\033[32m✓ Chaos test passed.\033[0m\n'
