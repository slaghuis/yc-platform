#!/usr/bin/env bash
# Compare schemas against a reference (e.g., origin/main) and flag breaking changes.
#
# Usage: ./check_backward_compat.sh [ref]
set -euo pipefail

REF="${1:-origin/main}"
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SCHEMAS="schemas/yc-event-schemas"

cd "$ROOT"

echo "▸ Comparing schemas against $REF..."

# For now, a conservative rule: fail if any existing versioned schema file was modified.
# Full semantic diff is future scope.
if git diff --name-only --diff-filter=MDR "$REF" -- "$SCHEMAS" | grep -E '\.v[0-9]+\.json$' > /tmp/changed.$$; then
    changed=$(cat /tmp/changed.$$)
    rm -f /tmp/changed.$$
    if [ -n "$changed" ]; then
        echo "✗ Versioned schema files modified. Create a new version instead:"
        echo "$changed" | sed 's/^/    /'
        exit 1
    fi
fi

echo "✓ No breaking changes detected (conservative check)."
