#!/usr/bin/env bash
# Generate RSA keypair for JWT signing in dev. Not for production.
set -euo pipefail

cd "$(dirname "$0")/.."

key_dir="deploy/compose/dev-keys"
mkdir -p "$key_dir"

priv="$key_dir/jwt-private.pem"
pub="$key_dir/jwt-public.pem"

if [ -f "$priv" ] && [ -f "$pub" ]; then
    echo "  Dev JWT keys already exist at $key_dir — skipping"
else
    echo "  Generating dev JWT RSA keypair..."
    openssl genrsa -out "$priv" 2048 2>/dev/null
    openssl rsa -in "$priv" -pubout -out "$pub" 2>/dev/null
    echo "  ✓ Dev JWT keys at $key_dir"
    echo "  ✗ DO NOT use these in production"
fi

# yc-identity runs as the image's nonroot user, not the host user that owns
# the bind-mounted key; on Linux hosts it can only read a world-readable file.
chmod 644 "$priv" "$pub"
