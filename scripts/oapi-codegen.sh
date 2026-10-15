#!/usr/bin/env bash
# Generates a service's server code from api/openapi.yaml with the pinned
# oapi-codegen (api-conventions.md §2). Run from the service directory:
#
#   cd services/yc-product && ../../scripts/oapi-codegen.sh
#
# The generator runs through `go run`, so it never becomes a dependency of
# the service module.
set -euo pipefail

OAPI_CODEGEN_VERSION=v2.8.0

if [[ ! -f api/openapi.yaml || ! -f api/oapi-codegen.yaml ]]; then
  echo "oapi-codegen.sh: run from a service directory with api/openapi.yaml and api/oapi-codegen.yaml" >&2
  exit 1
fi

GOWORK=off GOFLAGS=-mod=mod go run \
  "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}" \
  -config api/oapi-codegen.yaml api/openapi.yaml
