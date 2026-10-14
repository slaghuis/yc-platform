#!/usr/bin/env bash
# One-time setup after a fresh clone.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "▸ Checking required tools..."
./scripts/check-deps.sh

echo "▸ Installing pre-commit hooks..."
if command -v pre-commit >/dev/null 2>&1; then
    pre-commit install
else
    echo "  pre-commit not installed; skipping hook installation"
fi

echo "▸ Creating .env if missing..."
if [ ! -f .env ]; then
    cp .env.example .env
    echo "  Created .env from .env.example — review and adjust as needed"
fi

echo "▸ Generating JWT dev keys if missing..."
./scripts/gen-dev-jwt-keys.sh

echo "▸ Downloading Go dependencies..."
go work sync

echo "▸ Generating code..."
if [ -f tools/yc-schema-gen/main.go ]; then
    go run ./tools/yc-schema-gen || echo "  (schema generation skipped — tool not yet implemented)"
fi

echo ""
echo "✓ Bootstrap complete."
echo ""
echo "Next steps:"
echo "  task devstack:up    # start local Postgres, NATS, S3, Jaeger, MailHog"
echo "  task test           # run unit tests"
echo "  task --list         # see all available tasks"
