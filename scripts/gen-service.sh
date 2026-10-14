#!/usr/bin/env bash
# Scaffold a new service and wire it into the monorepo.
#
# Usage: ./scripts/gen-service.sh [--no-db] [--purpose "<one line>"] yc-foo
#
#   --no-db     The service has no database (no migrations, no DB config,
#               no entry in postgres-init.sh or .env.example).
#   --purpose   One-line description for the README.
#
# Creates services/<name>/ with go.mod, cmd/<name>/main.go, Taskfile.yml,
# config/config.default.yaml, README.md and (unless --no-db) the eventbus
# migrations, then adds the module to go.work. Compatible with bash 3.2.
set -euo pipefail

usage() {
    echo "Usage: $0 [--no-db] [--purpose \"<one line>\"] <service-name>"
    echo "Example: $0 yc-foo"
    exit 1
}

WITH_DB=1
PURPOSE=""
SERVICE=""
while [ $# -gt 0 ]; do
    case "$1" in
        --no-db) WITH_DB=0; shift ;;
        --purpose) [ $# -ge 2 ] || usage; PURPOSE="$2"; shift 2 ;;
        -*) usage ;;
        *) [ -z "$SERVICE" ] || usage; SERVICE="$1"; shift ;;
    esac
done
[ -n "$SERVICE" ] || usage

if [[ ! "$SERVICE" =~ ^yc-[a-z]+(-[a-z]+)*$ ]]; then
    echo "✗ Service name must match ^yc-[a-z]+(-[a-z]+)*\$ (got: $SERVICE)"
    exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVICE_DIR="$ROOT/services/$SERVICE"

if [ -e "$SERVICE_DIR" ]; then
    echo "✗ Service directory already exists: $SERVICE_DIR"
    exit 1
fi

MODULE_PATH="github.com/slaghuis/yc-platform/services/$SERVICE"
SHORT="${SERVICE#yc-}"
DB_NAME="yc_$(echo "$SHORT" | tr '-' '_')"
DB_ENV="$(echo "$SERVICE" | tr 'a-z-' 'A-Z_')_DB_URL"
GO_VERSION="$(sed -n 's/^go \([0-9.]*\).*/\1/p' "$ROOT/go.work")"
[ -n "$PURPOSE" ] || PURPOSE="<One-line service purpose.>"

echo "▸ Creating $SERVICE..."
mkdir -p "$SERVICE_DIR/cmd/$SERVICE" "$SERVICE_DIR/config"

cat > "$SERVICE_DIR/go.mod" <<EOF
module $MODULE_PATH

go $GO_VERSION
EOF

if [ $WITH_DB -eq 1 ]; then
    DB_README="Database: \`$DB_NAME\` (\`$DB_ENV\`)."
else
    DB_README="No database."
fi
cat > "$SERVICE_DIR/README.md" <<EOF
# $SERVICE

$PURPOSE

See \`docs/services/$SERVICE.md\` for the full requirement spec.

## Local development

\`\`\`bash
task run SERVICE=$SERVICE
\`\`\`

$DB_README
EOF

cat > "$SERVICE_DIR/Taskfile.yml" <<EOF
version: '3'

dotenv: ['../../.env']

vars:
  SERVICE: $SERVICE

tasks:
  build:
    desc: Build the service binary into bin/
    sources:
      - '**/*.go'
      - go.mod
    generates:
      - 'bin/{{.SERVICE}}'
    cmds:
      - go build -o bin/{{.SERVICE}} ./cmd/{{.SERVICE}}

  run:
    desc: Build and run locally (stops cleanly on Ctrl-C or SIGTERM)
    deps: [build]
    cmds:
      - ./bin/{{.SERVICE}}

  test:
    desc: Unit tests
    cmds:
      - go test ./...

  test:integration:
    desc: Integration tests (requires the dev stack)
    cmds:
      - go test -tags=integration ./...

  lint:
    desc: Lint this module
    cmds:
      - golangci-lint run

  docker:build:
    desc: Build the Docker image
    dir: ../..
    cmds:
      - docker build -t yc-platform/{{.SERVICE}}:dev -f deploy/docker/Dockerfile.service --build-arg SERVICE={{.SERVICE}} .
EOF

cat > "$SERVICE_DIR/cmd/$SERVICE/main.go" <<EOF
// Command $SERVICE runs the $SERVICE service.
//
// See docs/services/$SERVICE.md for the requirement spec.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).
		With("service", "$SERVICE", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("hello")
	<-ctx.Done()
	logger.Info("shutting down")
}
EOF

{
    echo "service: $SERVICE"
    echo "http:"
    echo "  listen: \":8080\""
    echo "log:"
    echo "  level: info"
    if [ $WITH_DB -eq 1 ]; then
        echo "database:"
        echo "  url: \${$DB_ENV}"
    fi
    echo "nats:"
    echo "  url: \${NATS_URL}"
    echo "telemetry:"
    echo "  otlp_endpoint: \${OTEL_EXPORTER_OTLP_ENDPOINT}"
} > "$SERVICE_DIR/config/config.default.yaml"

if [ $WITH_DB -eq 1 ]; then
    mkdir -p "$SERVICE_DIR/migrations"
    cp "$ROOT/libs/yc-eventbus/schema.sql" "$SERVICE_DIR/migrations/001_eventbus_tables.up.sql"
    cat > "$SERVICE_DIR/migrations/001_eventbus_tables.down.sql" <<'EOF'
DROP TABLE IF EXISTS processed_events;
DROP TABLE IF EXISTS outbox;
EOF

    INIT="$ROOT/deploy/compose/postgres-init.sh"
    if ! grep -qx "    $DB_NAME" "$INIT"; then
        DB_NAME="$DB_NAME" perl -0pi -e 's/(SERVICES=\(\n(?:    \S+\n)*)\)/$1    $ENV{DB_NAME}\n)/' "$INIT"
        echo "  ✓ Added $DB_NAME to deploy/compose/postgres-init.sh (run 'task devstack:reset' to create it)"
    fi

    ENV_EXAMPLE="$ROOT/.env.example"
    if ! grep -q "^$DB_ENV=" "$ENV_EXAMPLE"; then
        echo "$DB_ENV=postgres://${DB_NAME}_app:devpass@localhost:5432/$DB_NAME?sslmode=disable" >> "$ENV_EXAMPLE"
        echo "  ✓ Added $DB_ENV to .env.example (copy it to your .env)"
    fi
fi

(cd "$ROOT" && go work use "./services/$SERVICE")
echo "  ✓ Added ./services/$SERVICE to go.work"

echo "✓ Created services/$SERVICE"
echo ""
echo "Next steps:"
echo "  1. Write docs/services/$SERVICE.md"
echo "  2. task run SERVICE=$SERVICE"
