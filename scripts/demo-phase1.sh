#!/usr/bin/env bash
# Phase 1 milestone demo and exit-criteria drills (docs/implementation-plan.md).
#
#   task devstack:up && task devstack:app     # once
#   ./scripts/demo-phase1.sh [milestone|outbox|dlq|all]   (default: all)
#
# Needs curl, jq, uuidgen, docker and go. Talks to the services on their
# host ports (gateway 8090, identity 8081, monitor 8082).
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY_URL:-http://localhost:8090}
IDENTITY=${IDENTITY_URL:-http://localhost:8081}
MONITOR=${MONITOR_URL:-http://localhost:8082}
JAEGER=${JAEGER_URL:-http://localhost:16686}
TENANT=${TENANT:-demo-shop}
SHOP_HOST=${SHOP_HOST:-demo-shop.localhost}
export NATS_URL=${NATS_URL:-nats://localhost:4222}

# yc-monitor trusts identity headers from inside the platform network; the
# gateway does not route /sysadmin in Phase 1, so the demo calls it directly.
SYSADMIN=(-H "X-YC-Tenant-ID: _platform" -H "X-YC-Actor-ID: demo-admin" -H "X-YC-Actor-Type: system_admin")

step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }
uuid() { uuidgen | tr '[:upper:]' '[:lower:]'; }

# poll <seconds> <command...>: retry until the command succeeds.
poll() {
    local deadline=$((SECONDS + $1)); shift
    until "$@" >/dev/null 2>&1; do
        [ $SECONDS -lt $deadline ] || return 1
        sleep 0.5
    done
}

shop() { curl -sS -H "Host: $SHOP_HOST" "$@"; }

monitor_event_for() {
    curl -fsS "${SYSADMIN[@]}" "$MONITOR/sysadmin/events?tenant_id=$TENANT&limit=500" |
        jq -e --arg c "$1" '.events[] | select(.correlation_id == $c)'
}

identity_db() { docker exec yc-postgres psql -U postgres -d yc_identity -Atc "$1"; }

provision() {
    step "Provision tenant $TENANT ($SHOP_HOST) with yc-tenant-cli"
    go run ./tools/yc-tenant-cli provision --tenant-id "$TENANT" --name "Demo Shop" --hostname "$SHOP_HOST" |
        sed 's/^/    /'
    poll 15 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: $SHOP_HOST' $GATEWAY/me)\" = 200 ]" ||
        fail "gateway did not map $SHOP_HOST"
    ok "yc-gateway resolves $SHOP_HOST → $TENANT"
}

milestone() {
    step "1. /healthz on all three services"
    for url in "$GATEWAY" "$IDENTITY" "$MONITOR"; do
        curl -fsS "$url/healthz" >/dev/null || fail "$url/healthz"
        ok "$url/healthz"
    done

    provision

    step "2. Unauthenticated request through the gateway gets a visitor"
    CORR=$(uuid)
    local headers body
    headers=$(mktemp); body=$(mktemp)
    shop -D "$headers" -o "$body" -H "X-YC-Correlation-ID: $CORR" "$GATEWAY/me"
    grep -q '^HTTP/1.1 200' "$headers" || { cat "$headers" "$body"; fail "GET /me"; }
    COOKIE=$(grep -i '^set-cookie: yc_visitor=' "$headers" | sed -E 's/^[^:]+: ([^;]+).*/\1/' | tr -d '\r')
    [ -n "$COOKIE" ] || fail "no yc_visitor cookie"
    VISITOR=$(jq -r .user_id "$body")
    ok "200, Set-Cookie: ${COOKIE:0:40}… (HttpOnly)"
    ok "visitor $VISITOR in tenant $(jq -r .tenant_id "$body")"
    info "correlation ID $CORR"

    local again
    again=$(shop -H "Cookie: $COOKIE" "$GATEWAY/me" | jq -r .user_id)
    [ "$again" = "$VISITOR" ] || fail "cookie did not resume the visitor"
    ok "the cookie resumes the same visitor"

    step "3. identity.visitor.created.v1 recorded by yc-monitor"
    poll 15 monitor_event_for "$CORR" || fail "no event in yc-monitor for $CORR"
    monitor_event_for "$CORR" | jq -r '"    \(.event_type) from \(.source_service), event \(.event_id)"'
    ok "row in yc_monitor.received_events"

    step "4. Logs correlated across services"
    for svc in yc-gateway yc-identity yc-monitor; do
        local n
        n=$(docker logs "$svc" 2>&1 | grep -c "$CORR" || true)
        [ "$n" -gt 0 ] || fail "$svc has no log lines for $CORR"
        ok "$svc: $n log line(s) with correlation_id $CORR"
    done

    step "5. One trace spans gateway → identity (Jaeger)"
    local trace=""
    find_trace() {
        trace=$(curl -fsS -G "$JAEGER/api/traces" --data-urlencode service=yc-gateway \
                --data-urlencode lookback=1h --data-urlencode "tags={\"yc.correlation_id\":\"$CORR\"}" |
            jq -r '[.data[] | select([.processes[].serviceName] | index("yc-identity"))] | first | .traceID // empty')
        [ -n "$trace" ]
    }
    poll 20 find_trace || fail "no gateway→identity trace for $CORR in Jaeger"
    curl -fsS "$JAEGER/api/traces/$trace" |
        jq -r '.data[0] | "    services: \([.processes[].serviceName] | unique | join(", ")); spans: \(.spans | length)"'
    ok "$JAEGER/trace/$trace"
}

outbox() {
    step "Outbox drill: identity commits, crashes before relaying, restarts"
    info "restart yc-identity with its relay disabled"
    YC_IDENTITY__NATS__DISABLE_RELAY=true docker compose --profile app up -d yc-identity >/dev/null 2>&1
    poll 30 curl -fsS "$IDENTITY/readyz" || fail "yc-identity not ready"

    local corr pending
    corr=$(uuid)
    [ "$(shop -o /dev/null -w '%{http_code}' -H "X-YC-Correlation-ID: $corr" "$GATEWAY/me")" = 200 ] ||
        fail "visitor request failed"
    pending=$(identity_db "SELECT count(*) FROM outbox WHERE published_at IS NULL AND envelope->>'correlation_id' = '$corr'")
    [ "$pending" = 1 ] || fail "expected 1 pending outbox row, found $pending"
    ok "visitor committed; its event is pending in the outbox"
    sleep 2
    ! monitor_event_for "$corr" >/dev/null 2>&1 || fail "event escaped before the crash"
    ok "yc-monitor has not seen it"

    info "kill -9 yc-identity, then start it normally"
    docker kill yc-identity >/dev/null
    docker compose --profile app up -d yc-identity >/dev/null 2>&1
    poll 30 curl -fsS "$IDENTITY/readyz" || fail "yc-identity not ready after restart"
    poll 20 monitor_event_for "$corr" || fail "event never arrived after restart"
    ok "relay published it after restart; yc-monitor recorded it"
    [ "$(identity_db "SELECT count(*) FROM outbox WHERE published_at IS NULL AND envelope->>'correlation_id' = '$corr'")" = 0 ] ||
        fail "outbox row still pending"
    ok "outbox row marked published"
}

dlq() {
    step "DLQ drill: a schema-invalid event is dead-lettered"
    local event_id subject envelope
    event_id=$(uuid)
    subject="yc.$TENANT.identity.visitor.created.v1"
    envelope=$(jq -cn --arg id "$event_id" --arg t "$TENANT" --arg c "$(uuid)" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
        event_id: $id, event_type: "identity.visitor.created.v1", event_time: $now, tenant_id: $t,
        actor: {type: "service", id: "demo"}, correlation_id: $c, source_service: "demo",
        schema_version: 1, payload: {user_id: "not-a-uuid"}}')
    docker run --rm --network yc-platform_default natsio/nats-box:latest \
        nats --server nats://nats:4222 pub "$subject" "$envelope" >/dev/null 2>&1 || fail "nats-box publish"
    ok "published bad payload on $subject (event $event_id)"

    dlq_entry() {
        curl -fsS "${SYSADMIN[@]}" "$MONITOR/sysadmin/dlq?tenant_id=$TENANT&limit=500" |
            jq -e --arg id "$event_id" '.entries[] | select(.event_id == $id)'
    }
    poll 20 dlq_entry || fail "dead letter not recorded"
    dlq_entry | jq -r '"    consumer \(.consumer) (\(.service)), reason \(.failure_reason), seq \(.dlq_sequence)\n    error: \(.last_error | .[0:120])"'
    ok "landed on yc.$TENANT.identity.dlq.v1 and in yc_monitor.dlq_entries"
}

case "${1:-all}" in
    milestone) milestone ;;
    outbox)    outbox ;;
    dlq)       dlq ;;
    all)       milestone; outbox; dlq ;;
    *)         echo "usage: $0 [milestone|outbox|dlq|all]" >&2; exit 2 ;;
esac
printf '\n\033[32m✓ done\033[0m\n'
