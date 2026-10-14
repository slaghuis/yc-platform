#!/usr/bin/env bash
# Scaffold a new event schema.
#
# Usage: ./scripts/gen-event.sh <domain> <entity> <action> <version>
# Example: ./scripts/gen-event.sh order order placed 1
set -euo pipefail

if [ $# -lt 4 ]; then
    echo "Usage: $0 <domain> <entity> <action> <version>"
    echo "Example: $0 order order placed 1"
    exit 1
fi

DOMAIN="$1"
ENTITY="$2"
ACTION="$3"
VERSION="$4"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCHEMA_DIR="$ROOT/schemas/yc-event-schemas/$DOMAIN"
FILE="$SCHEMA_DIR/$ENTITY.$ACTION.v$VERSION.json"

if [ -f "$FILE" ]; then
    echo "✗ Schema already exists: $FILE"
    exit 1
fi

mkdir -p "$SCHEMA_DIR"

cat > "$FILE" <<EOF
{
  "\$schema": "https://json-schema.org/draft/2020-12/schema",
  "\$id": "https://yc/schemas/$DOMAIN/$ENTITY.$ACTION.v$VERSION.json",
  "title": "$DOMAIN.$ENTITY.$ACTION.v$VERSION",
  "description": "TODO: describe this event",
  "type": "object",
  "required": [],
  "additionalProperties": false,
  "properties": {}
}
EOF

echo "✓ Created $FILE"
echo ""
echo "Next steps:"
echo "  1. Fill in the schema properties and required fields"
echo "  2. Run: task gen:events"
echo "  3. Add entry to docs/event-taxonomy.md"
echo "  4. Update producer and consumer services"