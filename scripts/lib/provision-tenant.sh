# Shared by seed/demo/load scripts: provision a shop through
# POST /sysadmin/tenants (ADR-029). Source from the repository root after
# `set -euo pipefail`.
#
# Expects GATEWAY (or GATEWAY_URL) and ADMIN_HOST. Sets SYS to a system
# admin bearer token when it is unset. Extra arguments after the three
# positionals are forwarded to yc-tenant-cli (e.g. --store-currency).

ADMIN_HOST=${ADMIN_HOST:-admin.localhost}
SYSADMIN_EMAIL=${SYSADMIN_EMAIL:-sysadmin@yc.test}
SYSADMIN_PASSWORD=${SYSADMIN_PASSWORD:-correct-horse-battery}

# ensure_sysadmin signs in on the platform host and sets SYS.
ensure_sysadmin() {
	local gateway=${GATEWAY_URL:-${GATEWAY:-http://localhost:8090}}
	if [ -n "${SYS:-}" ]; then
		return 0
	fi
	docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD="$SYSADMIN_PASSWORD" yc-identity \
		/app/service bootstrap-admin --email "$SYSADMIN_EMAIL" >/dev/null
	local body status
	body=$(mktemp)
	status=$(curl -sS -o "$body" -w '%{http_code}' -X POST \
		-H "Host: $ADMIN_HOST" -H "Content-Type: application/json" \
		--data "{\"email\":\"$SYSADMIN_EMAIL\",\"password\":\"$SYSADMIN_PASSWORD\"}" \
		"$gateway/login")
	[ "$status" = 200 ] || {
		printf 'ensure_sysadmin: login failed (%s): %s\n' "$status" "$(cat "$body")" >&2
		rm -f "$body"
		return 1
	}
	SYS=$(jq -r .access_token <"$body")
	rm -f "$body"
	[ -n "$SYS" ] && [ "$SYS" != null ]
}

# platform_gateway_url is the gateway base URL with a platform Host
# (admin.localhost), so /sysadmin/tenants resolves.
platform_gateway_url() {
	if [ -n "${YC_GATEWAY_URL:-}" ]; then
		printf '%s\n' "$YC_GATEWAY_URL"
		return
	fi
	local gateway=${GATEWAY_URL:-${GATEWAY:-http://localhost:8090}}
	local port
	port=$(printf '%s' "$gateway" | sed -E 's|^https?://[^:]+:([0-9]+).*|\1|')
	[ -n "$port" ] || port=8090
	printf 'http://%s:%s\n' "$ADMIN_HOST" "$port"
}

# provision_tenant <tenant-id> <name> <hostname> [extra flags...]
provision_tenant() {
	local tenant=$1 name=$2 hostname=$3
	shift 3
	ensure_sysadmin
	go run ./tools/yc-tenant-cli provision \
		--token "$SYS" \
		--gateway-url "$(platform_gateway_url)" \
		--tenant-id "$tenant" \
		--name "$name" \
		--hostname "$hostname" \
		--contact-email "${CONTACT_EMAIL:-ops@${tenant}.test}" \
		"$@"
}
