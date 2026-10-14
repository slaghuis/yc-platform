#!/usr/bin/env bash
# Scaffold a new event schema.
#
# Usage: ./scripts/gen-event.sh <domain> [<entity>] <action> <version>
# Omit the entity when the event is about the domain's own aggregate.
# Examples:
#   ./scripts/gen-event.sh order placed 1               -> order/order.placed.v1.json
#   ./scripts/gen-event.sh inventory stock reserved 1   -> inventory/inventory.stock.reserved.v1.json
set -euo pipefail

usage() {
    echo "Usage: $0 <domain> [<entity>] <action> <version>"
    echo "Example: $0 order placed 1"
    echo "Example: $0 inventory stock reserved 1"
    exit 1
}

case $# in
    3) DOMAIN="$1"; ENTITY="";   ACTION="$2"; VERSION="$3" ;;
    4) DOMAIN="$1"; ENTITY="$2"; ACTION="$3"; VERSION="$4" ;;
    *) usage ;;
esac

if [ "$ENTITY" = "$DOMAIN" ]; then
    echo "✗ Entity '$ENTITY' repeats the domain; omit it (e.g. $DOMAIN.$ACTION.v$VERSION)"
    exit 1
fi

EVENT_TYPE="$DOMAIN${ENTITY:+.$ENTITY}.$ACTION.v$VERSION"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCHEMA_DIR="$ROOT/schemas/yc-event-schemas/$DOMAIN"
FILE="$SCHEMA_DIR/$EVENT_TYPE.json"

if [ -f "$FILE" ]; then
    echo "✗ Schema already exists: $FILE"
    exit 1
fi

mkdir -p "$SCHEMA_DIR"

cat > "$FILE" <<EOF
{
  "\$schema": "https://json-schema.org/draft/2020-12/schema",
  "\$id": "https://yc/schemas/$DOMAIN/$EVENT_TYPE.json",
  "title": "$EVENT_TYPE",
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
echo "  3. Add entry to docs/diagrams/event-taxonomy.md"
echo "  4. Update producer and consumer services"
