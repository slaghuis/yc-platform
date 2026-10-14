#!/usr/bin/env bash
# Verify required developer tools are installed.
set -euo pipefail

missing=0

check() {
    local name=$1
    local cmd=$2
    local version_cmd=${3-"--version"}
    if command -v "$cmd" >/dev/null 2>&1; then
        local version="installed"
        if [ -n "$version_cmd" ]; then
            version=$("$cmd" $version_cmd 2>&1 | head -1) || true
        fi
        printf "  ✓ %-25s %s\n" "$name" "$version"
    else
        printf "  ✗ %-25s NOT FOUND\n" "$name"
        missing=$((missing+1))
    fi
}

echo "Required tools:"
check "go"              go      "version"
check "docker"          docker
check "docker compose"  docker  "compose version"
check "task"            task    "--version"
check "git"             git
check "golangci-lint"   golangci-lint "version"

required_missing=$missing

echo ""
echo "Recommended tools:"
check "gofumpt"         gofumpt
check "goimports"       goimports ""
check "pre-commit"      pre-commit
check "migrate"         migrate  "-version"

gobin="$(go env GOPATH 2>/dev/null)/bin"
case ":$PATH:" in
    *":$gobin:"*) ;;
    *)
        if [ -d "$gobin" ]; then
            echo ""
            echo "  ! $gobin is not on PATH; tools installed with 'go install' won't be found."
            echo "    Add to your shell profile: export PATH=\"\$PATH:$gobin\""
        fi
        ;;
esac

missing=$required_missing
if [ $missing -gt 0 ]; then
    echo ""
    echo "✗ $missing required tool(s) missing."
    echo "  See docs/onboarding/developer-setup.md for installation instructions."
    exit 1
fi

echo ""
echo "✓ All required tools present."
