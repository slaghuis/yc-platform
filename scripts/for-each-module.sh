#!/usr/bin/env bash
# Run a command in every module listed in go.work.
#
# Usage: ./scripts/for-each-module.sh <command> [args...]
# Example: ./scripts/for-each-module.sh go test ./...
#
# Every module runs even if an earlier one fails; the script exits non-zero
# if any module failed and lists them.
set -uo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: $0 <command> [args...]"
    exit 2
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

failed=""
for dir in $(go list -m -f '{{.Dir}}'); do
    rel="${dir#"$ROOT"/}"
    echo "▸ $rel"
    if ! (cd "$dir" && "$@"); then
        failed="$failed $rel"
    fi
done

if [ -n "$failed" ]; then
    echo ""
    echo "✗ Failed in:$failed"
    exit 1
fi
