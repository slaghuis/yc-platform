#!/usr/bin/env bash
# Validate every JSON Schema file is syntactically correct.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCHEMAS="$ROOT/schemas/yc-event-schemas"

errors=0

if command -v jq >/dev/null 2>&1; then
    while IFS= read -r file; do
        if ! jq empty "$file" >/dev/null 2>&1; then
            echo "✗ Invalid JSON: $file"
            errors=$((errors+1))
        fi
    done < <(find "$SCHEMAS" -name '*.json' -type f)
else
    echo "  jq not installed; skipping JSON syntax validation"
fi

# TODO: full JSON Schema validation via a validator (ajv-cli or similar)

if [ $errors -gt 0 ]; then
    echo ""
    echo "✗ $errors schema file(s) failed validation."
    exit 1
fi

echo "✓ All schema files are valid JSON."