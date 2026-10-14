#!/usr/bin/env bash
# Scaffold a new service.
#
# Usage: ./scripts/gen-service.sh yc-foo
set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: $0 <service-name>"
    echo "Example: $0 yc-foo"
    exit 1
fi

SERVICE="$1"
if [[ ! "$SERVICE" =~ ^yc-[a-z]+$ ]]; then
    echo "✗ Service name must match ^yc-[a-z]+$ (got: $SERVICE)"
    exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVICE_DIR="$ROOT/services/$SERVICE"

if [ -d "$SERVICE_DIR" ]; then
    echo "✗ Service directory already exists: $SERVICE_DIR"
    exit 1
fi

MODULE_PATH="github.com/slaghuis/yc-platform/services/$SERVICE"
SHORT="${SERVICE#yc-}"
DB_NAME="yc_$(echo "$SHORT" | tr '-' '_')"

echo "▸ Creating service skeleton for $SERVICE..."

mkdir -p "$SERVICE_DIR"/{api,cmd/"$SERVICE",config,internal/{app,domain,service,repository,transport/{http,events},integration},migrations,tests/{unit,integration,testdata}}

# go.mod
cat > "$SERVICE_DIR/go.mod" <<EOF
module $MODULE_PATH

go 1.26
EOF

# README
cat > "$SERVICE_DIR/README.md" <<EOF
# $SERVICE

<One-line service purpose.>

See \`docs/services/$SERVICE.md\` for the full requirement spec.

## Local development

\`\`\`bash
task run SERVICE=$SERVICE
\`\`\`

## Database

Database: \`$DB_NAME\`

## Owned subjects

- \`yc.<tenant>.$SHORT.>\`
EOF

# Service Taskfile
cat > "$SERVICE_DIR/Taskfile.yml" <<EOF
version: '3'

vars:
  SERVICE: $SERVICE

tasks:
  build:
    desc: Build the service binary
    sources:
      - '**/*.go'
    generates:
      - 'bin/{{.SERVICE}}'
    cmds:
      - go build -o bin/{{.SERVICE}} ./cmd/{{.SERVICE}}

  run:
    desc: Run locally
    cmds:
      - go run ./cmd/{{.SERVICE}}

  test:
    desc: Unit tests
    cmds:
      - go test ./...

  test:integration:
    desc: Integration tests
    cmds:
      - go test -tags=integration ./...

  gen:openapi:
    desc: Regenerate OpenAPI handlers
    sources:
      - 'api/openapi.yaml'
    generates:
      - 'api/generated/*.go'
    cmds:
      - echo "oapi-codegen not configured for {{.SERVICE}} yet"
EOF

# Minimal main.go
cat > "$SERVICE_DIR/cmd/$SERVICE/main.go" <<EOF
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "time"
)

var version = "dev"

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    fmt.Fprintf(os.Stdout, "{\"msg\":\"service starting\",\"service\":\"$SERVICE\",\"version\":%q}\n", version)

    <-ctx.Done()

    shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancelShutdown()
    _ = shutdownCtx

    fmt.Fprintf(os.Stdout, "{\"msg\":\"service stopped\",\"service\":\"$SERVICE\"}\n")
}
EOF

# Config defaults
cat > "$SERVICE_DIR/config/config.default.yaml" <<EOF
service: $SERVICE
http:
  listen: ":8080"
log:
  level: info
database:
  url: \${${SHORT^^}_DB_URL:-postgres://localhost/$DB_NAME}
nats:
  url: \${NATS_URL:-nats://localhost:4222}
telemetry:
  otlp_endpoint: \${OTEL_EXPORTER_OTLP_ENDPOINT:-http://localhost:4318}
EOF

# Initial migrations — outbox + processed_events (standard across all services)
cat > "$SERVICE_DIR/migrations/001_eventbus_tables.up.sql" <<'EOF'
CREATE TABLE IF NOT EXISTS outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    subject         TEXT NOT NULL,
    envelope        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_outbox_unpublished
    ON outbox (next_attempt_at)
    WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS processed_events (
    event_id     UUID NOT NULL,
    consumer     TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_id, consumer)
);
CREATE INDEX IF NOT EXISTS idx_processed_events_time
    ON processed_events (processed_at);
EOF

cat > "$SERVICE_DIR/migrations/001_eventbus_tables.down.sql" <<'EOF'
DROP TABLE IF EXISTS processed_events;
DROP TABLE IF EXISTS outbox;
EOF

# Dockerfile symlink to shared
ln -s "../../deploy/docker/Dockerfile.service" "$SERVICE_DIR/Dockerfile"

echo "✓ Created $SERVICE_DIR"
echo ""
echo "Next steps:"
echo "  1. Add '$SERVICE' to go.work"
echo "  2. Add '$DB_NAME' to deploy/compose/postgres-init.sh"
echo "  3. Add '${SHORT^^}_DB_URL' to .env.example"
echo "  4. Create docs/services/$SERVICE.md"
echo "  5. Run: cd services/$SERVICE && go mod tidy"