# Monorepo Layout and Tooling

A comprehensive specification for the yc-platform monorepo, covering structure, shared libraries, build tooling, development workflow, and CI/CD. This is intended to be the single document that the AI code factory and human developers reference when working on the platform.

> The configuration files quoted below (`Taskfile.yml`, `docker-compose.yml`, `.golangci.yml`, `.editorconfig`, `.pre-commit-config.yaml`, workflows, scripts) show the design intent. Where a quoted file and the file in the repository differ, the repository file is authoritative.

---

## 1. Top-Level Directory Structure

```
yc-platform/
├── README.md
├── LICENSE
├── CODEOWNERS
├── .editorconfig
├── .gitignore
├── .gitattributes
├── .golangci.yml
├── .pre-commit-config.yaml
├── Taskfile.yml                     # Primary task runner (alternative to Make)
├── docker-compose.yml                # Dev stack (Postgres, NATS, SeaweedFS, etc.)
├── docker-compose.override.yml.example
│
├── go.work                           # Go workspace for multi-module monorepo
├── go.work.sum
│
├── docs/
│   ├── adr/                          # Architecture Decision Records
│   │   ├── 001-topic-based-tenant-isolation.md
│   │   ├── 002-message-bus-nats-jetstream.md
│   │   └── ...
│   ├── services/                     # Per-service requirement markdowns
│   │   ├── yc-identity.md
│   │   ├── yc-gateway.md
│   │   └── ...
│   ├── diagrams/                     # Architecture diagrams (mermaid, drawio)
│   │   └── checkout-saga.md
│   ├── standards/
│   │   ├── go-conventions.md
│   │   ├── api-conventions.md
│   │   ├── error-handling.md
│   │   ├── logging-tracing.md
│   │   ├── testing.md
│   │   └── event-conventions.md
│   ├── runbooks/                     # Operational procedures
│   │   ├── dlq-replay.md
│   │   ├── gdpr-incident-response.md
│   │   └── onboarding-tenant.md
│   └── onboarding/
│       ├── developer-setup.md
│       ├── adding-a-service.md
│       └── adding-an-event.md
│
├── schemas/
│   └── yc-event-schemas/             # JSON Schemas (as per ADR-007)
│       ├── envelope/
│       ├── common/
│       ├── identity/
│       ├── product/
│       └── ... (per domain)
│
├── libs/                             # Shared Go libraries
│   ├── yc-eventbus/
│   ├── yc-authctx/
│   ├── yc-httpkit/
│   ├── yc-dbkit/
│   ├── yc-telemetry/
│   ├── yc-config/
│   ├── yc-errors/
│   ├── yc-money/
│   ├── yc-testkit/
│   └── yc-events/                    # Generated Go types from schemas
│
├── services/                         # One directory per service
│   ├── yc-identity/
│   ├── yc-gateway/
│   ├── yc-image/
│   ├── yc-preference/
│   ├── yc-product/
│   ├── yc-pricing/
│   ├── yc-inventory/
│   ├── yc-cart/
│   ├── yc-review/
│   ├── yc-checkout/
│   ├── yc-payment/
│   ├── yc-shipping/
│   ├── yc-orders/
│   ├── yc-message/
│   ├── yc-audit/
│   ├── yc-monitor/
│   └── yc-analytics/
│
├── tools/                            # Developer and operational tools
│   ├── yc-dlq-cli/                   # DLQ inspection/replay
│   ├── yc-tenant-cli/                # Tenant provisioning
│   ├── yc-schema-gen/                # Generate Go types from JSON Schema
│   ├── yc-migrate/                   # Migration helper (wraps golang-migrate)
│   └── yc-devstack/                  # Spin up local dev environment
│
├── deploy/                           # Deployment artifacts
│   ├── docker/
│   │   └── Dockerfile.service        # Shared service Dockerfile
│   ├── compose/                      # Compose files for integrated testing
│   ├── k8s/                          # Future: Kubernetes manifests / Helm charts
│   └── terraform/                    # Future: infrastructure as code
│
├── test/                             # Cross-service tests
│   ├── e2e/                          # End-to-end tests (full saga)
│   ├── integration/                  # Multi-service integration tests
│   └── fixtures/                     # Shared test data
│
├── .github/
│   └── workflows/
│       ├── ci.yml
│       ├── schema-check.yml
│       ├── e2e.yml
│       └── release.yml
│
└── scripts/
    ├── bootstrap.sh                  # One-time setup for a fresh clone
    ├── gen-service.sh                # Scaffold a new service
    ├── gen-event.sh                  # Scaffold a new event schema + Go type
    └── check-deps.sh                 # Verify required tools installed
```

---

## 2. Per-Service Directory Structure

Every service follows the identical layout. The AI code factory and humans should rely on this uniformity.

```
services/yc-identity/
├── README.md                         # Service purpose, local run instructions
├── go.mod                            # Service is its own Go module
├── go.sum
├── Dockerfile                        # Optionally overrides shared Dockerfile
├── Taskfile.yml                      # Service-specific tasks
│
├── api/
│   ├── openapi.yaml                  # Service's OpenAPI spec
│   └── generated/                    # Generated handlers/clients (gitignored or committed per policy)
│
├── cmd/
│   └── yc-identity/
│       └── main.go                   # Entry point — thin; delegates to internal/app
│
├── internal/                         # All non-public code under internal/
│   ├── app/
│   │   ├── app.go                    # Composition root; builds dependencies
│   │   ├── config.go                 # Config struct + loader
│   │   └── wire.go                   # DI wiring (manual or via wire)
│   ├── domain/                       # Pure domain types, no infrastructure
│   │   ├── user.go
│   │   ├── token.go
│   │   └── errors.go
│   ├── service/                      # Use case orchestration
│   │   ├── registration.go
│   │   ├── authentication.go
│   │   └── gdpr.go
│   ├── repository/                   # Persistence
│   │   ├── user_repository.go
│   │   └── oauth_repository.go
│   ├── transport/
│   │   ├── http/                     # HTTP handlers
│   │   │   ├── router.go
│   │   │   ├── middleware.go
│   │   │   ├── user_handler.go
│   │   │   └── admin_handler.go
│   │   └── events/                   # Event subscribers
│   │       ├── gdpr_cascade_handler.go
│   │       └── tenant_lifecycle_handler.go
│   └── integration/                  # External service clients
│       ├── oauth_google.go
│       └── oauth_facebook.go
│
├── migrations/                       # golang-migrate SQL migrations
│   ├── 001_initial_schema.up.sql
│   ├── 001_initial_schema.down.sql
│   ├── 002_add_pseudonym_map.up.sql
│   └── 002_add_pseudonym_map.down.sql
│
├── events/                           # Published event definitions (Go code generated from schemas lives in /libs/yc-events)
│   └── published.go                  # Local wiring: event type constants per service
│
├── config/
│   ├── config.default.yaml           # Default config values
│   └── config.dev.yaml               # Dev overrides (committed; secrets externalized)
│
└── tests/
    ├── unit/
    ├── integration/
    └── testdata/
```

### Why `internal/`?

Go's `internal/` prevents other services from importing the service's packages. Each service owns its code — shared code must live in `/libs/`. This enforces the microservice boundary at the compiler level.

---

## 3. Shared Libraries (`/libs/`)

Each library is a separate Go module. Services import libraries via the Go workspace (`go.work`), so local changes are picked up immediately.

### `yc-eventbus`
NATS JetStream wrapper, outbox relay, consumer idempotency, DLQ handling, schema validation. See its service requirement doc.

### `yc-authctx`
Access token parsing and the `AuthContext` type that flows through every handler. Reads `X-YC-Tenant-ID`, `X-YC-Correlation-ID`, `X-YC-Actor-*` headers from yc-gateway. Provides middleware for HTTP handlers.

```go
type AuthContext struct {
    TenantID       string
    ActorID        string
    ActorType      string          // visitor, customer, shop_admin, system_admin
    Role           string
    CorrelationID  string
    AccessTokenJTI string
}

func FromContext(ctx context.Context) (AuthContext, bool)
func Middleware() func(http.Handler) http.Handler
```

### `yc-httpkit`
HTTP server setup, standard middleware chain, graceful shutdown, request logging, OpenTelemetry instrumentation, standard error responses, pagination helpers, standard health endpoints.

```go
type Server struct { ... }

func NewServer(cfg ServerConfig, routes RouterFunc) *Server
func (s *Server) Run(ctx context.Context) error

// Standard problem+json error responses
func WriteError(w http.ResponseWriter, err error)
```

### `yc-dbkit`
Postgres connection setup, PgBouncer-friendly configuration, migration runner, RLS session configuration, transaction helpers, standard outbox table bootstrap.

```go
func OpenDB(cfg DBConfig) (*pgxpool.Pool, error)
func RunMigrations(dsn, migrationsDir string) error
func WithTenantRLS(ctx context.Context, tx pgx.Tx, tenantID string) error
```

### `yc-telemetry`
OpenTelemetry setup (traces + metrics), logger (structured JSON), log/trace correlation. Exposes a single `Init` that services call at startup.

```go
func Init(ctx context.Context, serviceName string, cfg TelemetryConfig) (shutdown func(context.Context) error, err error)

var Logger *slog.Logger    // configured with trace context automatic enrichment
```

### `yc-config`
Config loading from YAML + environment variable overrides, with validation. Pattern: each service defines its own `Config` struct and calls `yc-config` to load it.

```go
func Load(configPath string, into any) error    // overlays YAML + env
```

### `yc-errors`
Error taxonomy aligned with HTTP semantics (not generic). Each error carries a code, a user-safe message, and optional details.

```go
var (
    ErrNotFound       = Error{Code: "not_found", Status: 404}
    ErrConflict       = Error{Code: "conflict", Status: 409}
    ErrForbidden      = Error{Code: "forbidden", Status: 403}
    ErrValidation     = Error{Code: "validation_failed", Status: 400}
    ErrInternal       = Error{Code: "internal", Status: 500}
    ErrUpstream       = Error{Code: "upstream_failure", Status: 502}
)

func Wrap(err error, msg string) error
func Code(err error) string
```

### `yc-money`
Money arithmetic in minor units. No floats. Currency-aware addition, subtraction, allocation (splitting amounts without rounding loss).

```go
type Money struct {
    AmountMinor int64  `json:"amount_minor"`
    Currency    string `json:"currency"`
}

func (m Money) Add(other Money) (Money, error)            // ErrCurrencyMismatch, ErrOverflow
func (m Money) Times(qty int64) (Money, error)
func (m Money) Mul(r Rate) (Money, error)                 // percentages; rounds half away from zero
func (m Money) Convert(to string, r Rate) (Money, error)  // exchange rates in major units; handles JPY/KWD exponents
func Allocate(total Money, weights []int64) ([]Money, error) // largest remainder; sum == total guaranteed

type Rate struct{ /* exact decimal, math/big.Rat */ }
func ParseRate(s string) (Rate, error)                    // "0.15"; JSON number or string
```

Rates are exact decimals (`math/big`), never floats, so the library has no third-party dependencies.

### `yc-events`
Generated Go types for every event in `yc-event-schemas`, plus the schema `Validator` passed to yc-eventbus. `task gen:events` (`tools/yc-schema-gen`) writes `types_gen.go` and the embedded `schemas/` copy. Services import these types rather than hand-crafting JSON.

```go
package events

type OrderPlacedV1 struct {
    OrderID     string      `json:"order_id"`
    ...
}

const (
    TypeOrderPlacedV1   = "order.placed.v1"
    TypeOrderShippedV1  = "order.shipped.v1"
    // ...
)
```

### `yc-testkit`
Test helpers used by every service:
- Postgres test container boot
- NATS test container boot
- HTTP test server with auth middleware
- Fake AuthContext builder
- Event publishing/subscribing helpers for assertions
- Golden file comparison

---

## 4. Go Workspace Configuration

`go.work` at the repo root ties all modules together:

```go
go 1.26

use (
    ./libs/yc-eventbus
    ./libs/yc-authctx
    ./libs/yc-httpkit
    ./libs/yc-dbkit
    ./libs/yc-telemetry
    ./libs/yc-config
    ./libs/yc-errors
    ./libs/yc-money
    ./libs/yc-testkit
    ./libs/yc-events

    ./services/yc-identity
    ./services/yc-gateway
    ./services/yc-image
    ./services/yc-preference
    ./services/yc-product
    ./services/yc-pricing
    ./services/yc-inventory
    ./services/yc-cart
    ./services/yc-review
    ./services/yc-checkout
    ./services/yc-payment
    ./services/yc-shipping
    ./services/yc-orders
    ./services/yc-message
    ./services/yc-audit
    ./services/yc-monitor
    ./services/yc-analytics

    ./tools/yc-dlq-cli
    ./tools/yc-tenant-cli
    ./tools/yc-schema-gen
    ./tools/yc-migrate
    ./tools/yc-devstack
)
```

### Why one module per service and per lib?

- **Independent versioning** (we don't need this yet, but it's cheap to have)
- **Independent dependency graphs** — a service pulling in a heavy dependency doesn't affect others
- **Easy future split** — when the time comes to break the monorepo, modules are already defined
- **CI efficiency** — build/test only modules affected by a change

### Module naming

- Repo: `github.com/slaghuis/yc-platform`
- Each module: `github.com/slaghuis/yc-platform/<path>`
  - e.g., `github.com/slaghuis/yc-platform/libs/yc-eventbus`
  - e.g., `github.com/slaghuis/yc-platform/services/yc-identity`

---

## 5. Build Tooling — Taskfile

We use **[Task](https://taskfile.dev/)** as the primary task runner. It is cross-platform, YAML-based, simpler than Make, and has native support for file-based change detection.

### Root `Taskfile.yml`

```yaml
version: '3'

vars:
  GO_VERSION: '1.26'
  SERVICES: >-
    yc-identity yc-gateway yc-image yc-preference yc-product
    yc-pricing yc-inventory yc-cart yc-review yc-checkout
    yc-payment yc-shipping yc-orders yc-message yc-audit
    yc-monitor yc-analytics

tasks:
  default:
    desc: Show help
    cmd: task --list

  bootstrap:
    desc: One-time setup after fresh clone
    cmd: ./scripts/bootstrap.sh

  check-deps:
    desc: Verify required tools are installed
    cmd: ./scripts/check-deps.sh

  # ─── Code generation ──────────────────────────────────────
  gen:
    desc: Regenerate all generated code
    deps: [gen:events, gen:openapi]

  gen:events:
    desc: Regenerate Go types from JSON schemas
    sources:
      - 'schemas/yc-event-schemas/**/*.json'
    generates:
      - 'libs/yc-events/**/*.go'
    cmd: go run ./tools/yc-schema-gen

  gen:openapi:
    desc: Regenerate OpenAPI handlers/clients for all services
    cmds:
      - for: { var: SERVICES }
        cmd: task -d services/{{.ITEM}} gen:openapi

  # ─── Linting and formatting ───────────────────────────────
  lint:
    desc: Run golangci-lint in every module in go.work
    cmd: ./scripts/for-each-module.sh golangci-lint run

  fmt:
    desc: Format Go code
    cmds:
      - gofumpt -w .
      - goimports -w .

  schema:validate:
    desc: Validate JSON schemas
    cmd: ./schemas/yc-event-schemas/scripts/validate_all.sh

  schema:compat:
    desc: Check backward compatibility of schemas
    cmd: ./schemas/yc-event-schemas/scripts/check_backward_compat.sh

  # ─── Testing ──────────────────────────────────────────────
  test:
    desc: Run all unit tests
    cmd: go test ./...

  test:integration:
    desc: Run integration tests (requires devstack)
    deps: [devstack:up]
    cmd: go test -tags=integration ./...

  test:e2e:
    desc: Run end-to-end tests
    deps: [devstack:up]
    cmd: go test -tags=e2e ./test/e2e/...

  # ─── Build ────────────────────────────────────────────────
  build:
    desc: Build all services
    cmds:
      - for: { var: SERVICES }
        cmd: task -d services/{{.ITEM}} build

  build:service:
    desc: Build a single service (SERVICE=yc-identity)
    cmd: task -d services/{{.SERVICE}} build

  # ─── Docker images ───────────────────────────────────────
  docker:build:
    desc: Build Docker images for all services
    cmds:
      - for: { var: SERVICES }
        cmd: task -d services/{{.ITEM}} docker:build

  # ─── Dev stack ────────────────────────────────────────────
  devstack:up:
    desc: Start local Postgres, NATS, S3 (SeaweedFS)
    cmd: docker compose up -d

  devstack:down:
    desc: Stop local dev stack
    cmd: docker compose down

  devstack:reset:
    desc: Wipe and restart dev stack (destroys data)
    cmd: docker compose down -v && docker compose up -d

  # ─── Migrations ───────────────────────────────────────────
  migrate:up:
    desc: Apply all migrations for all services
    cmd: go run ./tools/yc-migrate up --all

  migrate:down:
    desc: Roll back one migration for SERVICE
    cmd: go run ./tools/yc-migrate down --service={{.SERVICE}}

  migrate:new:
    desc: Create new migration (SERVICE=... NAME=...)
    cmd: go run ./tools/yc-migrate new --service={{.SERVICE}} --name={{.NAME}}

  # ─── Scaffolding ──────────────────────────────────────────
  scaffold:service:
    desc: Create a new service skeleton (NAME=yc-foo)
    cmd: ./scripts/gen-service.sh {{.NAME}}

  scaffold:event:
    desc: Create a new event schema (DOMAIN=order ACTION=placed VERSION=1; add ENTITY=stock for non-root entities)
    cmd: ./scripts/gen-event.sh {{.DOMAIN}} {{.ENTITY}} {{.ACTION}} {{.VERSION}}

  # ─── Convenience ──────────────────────────────────────────
  run:
    desc: Run a service locally (SERVICE=yc-identity)
    cmd: task -d services/{{.SERVICE}} run

  clean:
    desc: Clean all build artifacts
    cmd: go clean -cache -testcache ./...
```

### Per-Service `Taskfile.yml`

```yaml
version: '3'

vars:
  SERVICE: yc-identity

tasks:
  build:
    desc: Build the service binary
    sources:
      - '**/*.go'
    generates:
      - 'bin/{{.SERVICE}}'
    cmd: go build -o bin/{{.SERVICE}} ./cmd/{{.SERVICE}}

  run:
    desc: Run locally
    cmd: go run ./cmd/{{.SERVICE}}

  test:
    desc: Unit tests
    cmd: go test ./...

  test:integration:
    desc: Integration tests
    cmd: go test -tags=integration ./...

  gen:openapi:
    desc: Regenerate OpenAPI handlers
    sources:
      - 'api/openapi.yaml'
    generates:
      - 'api/generated/*.go'
    cmd: oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

  docker:build:
    desc: Build Docker image
    cmd: docker build -t yc-platform/{{.SERVICE}}:dev -f ../../deploy/docker/Dockerfile.service --build-arg SERVICE={{.SERVICE}} ../..

  migrate:up:
    desc: Apply migrations to this service's database
    cmd: migrate -database $YC_IDENTITY_DB_URL -path ./migrations up

  migrate:down:
    desc: Roll back one migration
    cmd: migrate -database $YC_IDENTITY_DB_URL -path ./migrations down 1
```

---

## 6. Docker Compose — Local Dev Stack

### `docker-compose.yml`

```yaml
version: '3.9'

services:
  postgres:
    image: postgres:16
    restart: unless-stopped
    environment:
      POSTGRES_PASSWORD: devpass
      POSTGRES_DB: postgres
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./deploy/compose/postgres-init.sh:/docker-entrypoint-initdb.d/init.sh:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      retries: 10

  nats:
    image: nats:2.10-alpine
    restart: unless-stopped
    command:
      - "--jetstream"
      - "--store_dir=/data"
      - "--http_port=8222"
    ports:
      - "4222:4222"
      - "8222:8222"
    volumes:
      - natsdata:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://localhost:8222/healthz"]
      interval: 5s
      retries: 10

  seaweedfs:
    image: chrislusf/seaweedfs:4.48
    restart: unless-stopped
    command: ["mini", "-dir=/data", "-bucket=yc-images", "-s3.config=/etc/seaweedfs/s3.json"]
    ports:
      - "9000:8333"        # S3 API
      - "9001:23646"       # Admin UI
    volumes:
      - s3data:/data
      - ./deploy/compose/seaweedfs-s3.json:/etc/seaweedfs/s3.json:ro
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://localhost:8333/healthz"]
      interval: 5s
      retries: 10

  jaeger:
    image: jaegertracing/all-in-one:latest
    restart: unless-stopped
    environment:
      COLLECTOR_OTLP_ENABLED: "true"
    ports:
      - "16686:16686"      # UI
      - "4317:4317"        # OTLP gRPC
      - "4318:4318"        # OTLP HTTP

  mailhog:
    image: mailhog/mailhog:latest
    restart: unless-stopped
    ports:
      - "1025:1025"        # SMTP
      - "8025:8025"        # UI

volumes:
  pgdata:
  natsdata:
  s3data:
```

### `postgres-init.sh`

Creates one database and one user per service:

```bash
#!/bin/bash
set -e

SERVICES=(
  yc_identity yc_image yc_preference yc_product yc_pricing
  yc_inventory yc_cart yc_review yc_checkout yc_payment
  yc_shipping yc_orders yc_message yc_audit yc_monitor yc_analytics yc_tenant yc_gateway
)

for svc in "${SERVICES[@]}"; do
  psql -v ON_ERROR_STOP=1 --username postgres <<-EOSQL
    CREATE USER ${svc}_app WITH PASSWORD 'devpass';
    CREATE DATABASE ${svc} OWNER ${svc}_app;
    GRANT ALL PRIVILEGES ON DATABASE ${svc} TO ${svc}_app;
EOSQL
done
```

### `docker-compose.override.yml.example`

Local developer overrides. Developers copy to `docker-compose.override.yml` (gitignored):

```yaml
version: '3.9'
services:
  postgres:
    ports:
      - "15432:5432"       # avoid clash with host Postgres
```

---

## 7. Shared Service Dockerfile

`deploy/docker/Dockerfile.service` — one Dockerfile for all services, parameterized by `SERVICE`:

```dockerfile
# syntax=docker/dockerfile:1.6
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder
ARG SERVICE
WORKDIR /src
RUN apk add --no-cache git

# Copy workspace definition and all go.mod/go.sum first for cache efficiency
COPY go.work go.work.sum ./
COPY libs/ libs/
COPY services/ services/
COPY tools/ tools/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd services/${SERVICE} && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/service ./cmd/${SERVICE}

# ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ARG SERVICE
LABEL org.opencontainers.image.title="${SERVICE}"
LABEL org.opencontainers.image.source="https://github.com/slaghuis/yc-platform"

COPY --from=builder /out/service /app/service
USER nonroot:nonroot
ENTRYPOINT ["/app/service"]
```

- **Distroless** for minimal attack surface
- **Nonroot** user by default
- **Static binary** (CGO disabled)
- **Build cache mounts** keep rebuilds fast

---

## 8. Code Generation

### Event types from JSON schemas

`tools/yc-schema-gen` reads `schemas/yc-event-schemas/` and writes Go structs to `libs/yc-events/`. Trigger: `task gen:events`.

Rules:
- One Go file per schema version
- Package per domain (e.g., `events/order/`, `events/identity/`)
- Each generated file has a `// Code generated — DO NOT EDIT` header
- CI fails if generated files are out of sync with schemas (`go generate ./...` and `git diff --exit-code`)

### OpenAPI handlers and clients

Each service's `api/openapi.yaml` is the contract. We use **[oapi-codegen](https://github.com/deepmap/oapi-codegen)** to generate:
- Server interfaces (handlers)
- Client code for use by other services
- Type structs for requests/responses

`services/yc-identity/api/oapi-codegen.yaml`:

```yaml
package: generated
output: api/generated/server.gen.go
generate:
  chi-server: true
  models: true
  embedded-spec: true
  strict-server: true
```

Trigger: `task gen:openapi` (in service dir) or `task gen` (root).

---

## 9. Lint and Format Standards

### `.golangci.yml`

```yaml
run:
  timeout: 5m
  tests: true
  go: '1.26'

linters:
  enable:
    - errcheck
    - gosimple
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gofumpt
    - goimports
    - revive
    - gosec
    - bodyclose
    - rowserrcheck
    - sqlclosecheck
    - noctx
    - exhaustive
    - prealloc
    - unconvert
    - unparam
    - whitespace
    - misspell
    - nolintlint
    - errorlint
    - nilerr
    - contextcheck

linters-settings:
  revive:
    rules:
      - name: exported
      - name: var-naming
      - name: package-comments
  gosec:
    excludes:
      - G101   # false positives on config fields
  misspell:
    locale: UK

issues:
  exclude-rules:
    - path: _test\.go
      linters:
        - gosec
        - errcheck
    - path: 'api/generated/'
      linters:
        - all
    - path: 'libs/yc-events/'
      linters:
        - all
```

### Formatters

- `gofumpt` (stricter `gofmt`)
- `goimports` (sorted imports with local prefix)

`.editorconfig`:

```ini
root = true

[*]
indent_style = space
indent_size = 4
end_of_line = lf
charset = utf-8
trim_trailing_whitespace = true
insert_final_newline = true

[*.go]
indent_style = tab
indent_size = 4

[*.{yml,yaml,json}]
indent_size = 2

[Makefile]
indent_style = tab
```

### Pre-commit

`.pre-commit-config.yaml`:

```yaml
repos:
  - repo: https://github.com/pre-commit/pre-commit-hooks
    rev: v4.5.0
    hooks:
      - id: trailing-whitespace
      - id: end-of-file-fixer
      - id: check-yaml
      - id: check-added-large-files

  - repo: local
    hooks:
      - id: gofumpt
        name: gofumpt
        language: system
        entry: gofumpt -w
        files: \.go$

      - id: goimports
        name: goimports
        language: system
        entry: goimports -w -local github.com/slaghuis/yc-platform
        files: \.go$

      - id: golangci-lint
        name: golangci-lint
        language: system
        entry: golangci-lint run --fix
        pass_filenames: false

      - id: schema-validate
        name: validate event schemas
        language: system
        entry: ./schemas/yc-event-schemas/scripts/validate_all.sh
        pass_filenames: false
        files: ^schemas/
```

---

## 10. CI/CD — GitHub Actions

### `.github/workflows/ci.yml`

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

env:
  GO_VERSION: '1.26'

jobs:
  changes:
    name: Detect changes
    runs-on: ubuntu-latest
    outputs:
      services: ${{ steps.filter.outputs.changes }}
      libs: ${{ steps.libs.outputs.changes }}
      schemas: ${{ steps.schemas.outputs.any_changed }}
    steps:
      - uses: actions/checkout@v4
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            yc-identity:  services/yc-identity/**
            yc-gateway:   services/yc-gateway/**
            yc-image:     services/yc-image/**
            # ... one per service
      - uses: dorny/paths-filter@v3
        id: libs
        with:
          filters: |
            libs: libs/**
      - uses: dorny/paths-filter@v3
        id: schemas
        with:
          filters: |
            schemas: schemas/**

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '${{ env.GO_VERSION }}' }
      - uses: golangci/golangci-lint-action@v9
        with:
          version: v2.14.0
          install-only: true      # no root module; task lint runs it per module
      - run: task lint

  schema-check:
    runs-on: ubuntu-latest
    needs: changes
    if: needs.changes.outputs.schemas == 'true'
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version: '${{ env.GO_VERSION }}' }
      - name: Validate schemas
        run: ./schemas/yc-event-schemas/scripts/validate_all.sh
      - name: Check backward compatibility
        run: ./schemas/yc-event-schemas/scripts/check_backward_compat.sh origin/main
      - name: Verify generated types up-to-date
        run: |
          go run ./tools/yc-schema-gen
          git diff --exit-code libs/yc-events/

  test-libs:
    runs-on: ubuntu-latest
    needs: [changes, lint]
    if: needs.changes.outputs.libs == 'true'
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '${{ env.GO_VERSION }}' }
      - name: Test all libs
        run: |
          for lib in libs/*/; do
            (cd "$lib" && go test ./...)
          done

  test-services:
    runs-on: ubuntu-latest
    needs: [changes, lint]
    strategy:
      fail-fast: false
      matrix:
        service: ${{ fromJson(needs.changes.outputs.services) }}
    services:
      postgres:
        image: postgres:16
        env: { POSTGRES_PASSWORD: devpass }
        ports: ['5432:5432']
        options: >-
          --health-cmd pg_isready --health-interval 5s --health-retries 10
      nats:
        image: nats:2.10-alpine
        ports: ['4222:4222']
        options: --health-cmd "nats-server --help"
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '${{ env.GO_VERSION }}' }
      - name: Unit tests
        run: cd services/${{ matrix.service }} && go test ./...
      - name: Integration tests
        run: cd services/${{ matrix.service }} && go test -tags=integration ./...

  build-images:
    runs-on: ubuntu-latest
    needs: [test-services]
    if: github.ref == 'refs/heads/main'
    strategy:
      matrix:
        service: ${{ fromJson(needs.changes.outputs.services) }}
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v5
        with:
          context: .
          file: deploy/docker/Dockerfile.service
          build-args: SERVICE=${{ matrix.service }}
          push: true
          tags: |
            ghcr.io/slaghuis/yc-platform/${{ matrix.service }}:${{ github.sha }}
            ghcr.io/slaghuis/yc-platform/${{ matrix.service }}:latest
          cache-from: type=gha,scope=${{ matrix.service }}
          cache-to: type=gha,scope=${{ matrix.service }},mode=max
```

### Key CI principles

- **Build only what changed** — detect which services/libs changed, test only those
- **Matrix for parallelism** — one job per changed service
- **Fail fast on lint and schema** — before expensive integration tests
- **Image build only on main** — PRs test code; main builds artifacts
- **Backward compat check** — schemas changed in a PR must not break v1 schemas

### `.github/workflows/e2e.yml`

Separate workflow for full end-to-end tests. Runs on main merge and nightly:

```yaml
name: E2E

on:
  push:
    branches: [main]
  schedule:
    - cron: '0 2 * * *'

jobs:
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.26' }
      - name: Start full stack
        run: docker compose -f docker-compose.yml -f deploy/compose/e2e.yml up -d
      - name: Wait for services
        run: ./scripts/wait-for-stack.sh
      - name: Run E2E tests
        run: go test -tags=e2e -v ./test/e2e/...
      - name: Collect logs on failure
        if: failure()
        run: docker compose logs > docker-logs.txt
      - uses: actions/upload-artifact@v4
        if: failure()
        with:
          name: docker-logs
          path: docker-logs.txt
```

---

## 11. Scaffolding — Adding a New Service

The Phase 0 scaffold (`scripts/gen-service.sh`, described in `adding-a-service.md`) creates a minimal service that logs and stops cleanly, registers it in `go.work`, and wires its database into `postgres-init.sh` and `.env.example`. Once the shared libraries exist (Phase 1), the scaffold grows to the full target below.

The target `scripts/gen-service.sh <service-name>` does the following:

1. Create `services/<name>/` with the full directory layout
2. Generate `go.mod` with correct module path
3. Add service to `go.work`
4. Create a minimal `cmd/<name>/main.go` that:
   - Loads config via `yc-config`
   - Initializes telemetry via `yc-telemetry`
   - Opens DB via `yc-dbkit`
   - Starts eventbus client via `yc-eventbus`
   - Starts HTTP server via `yc-httpkit`
   - Registers with yc-gateway
   - Handles graceful shutdown
5. Create a stub OpenAPI spec at `api/openapi.yaml`
6. Create the standard `outbox` and `processed_events` migrations
7. Create a `Dockerfile` symlink or override
8. Add service to `docker-compose.yml` (as commented-out block)
9. Add service name to CI matrix path filter
10. Register the database in `postgres-init.sh`

Minimal `main.go` template (abbreviated):

```go
package main

import (
    "context"
    "log/slog"
    "os"
    "os/signal"
    "syscall"

    "github.com/slaghuis/yc-platform/libs/yc-config"
    "github.com/slaghuis/yc-platform/libs/yc-dbkit"
    "github.com/slaghuis/yc-platform/libs/yc-eventbus"
    "github.com/slaghuis/yc-platform/libs/yc-httpkit"
    "github.com/slaghuis/yc-platform/libs/yc-telemetry"

    "github.com/slaghuis/yc-platform/services/yc-identity/internal/app"
)

var version = "dev"

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    cfg, err := config.Load[app.Config]("config/config.yaml")
    must(err)

    shutdownTelemetry, err := telemetry.Init(ctx, "yc-identity", cfg.Telemetry)
    must(err)
    defer shutdownTelemetry(context.Background())

    db, err := dbkit.OpenDB(cfg.Database)
    must(err)
    defer db.Close()

    bus, err := eventbus.NewClient(ctx, eventbus.Config{
        NATSUrl:        cfg.NATS.URL,
        ServiceName:    "yc-identity",
        OutboxDBHandle: db,
    })
    must(err)
    defer bus.Close()

    application, err := app.New(cfg, db, bus)
    must(err)

    server := httpkit.NewServer(cfg.HTTP, application.Routes)

    if err := server.Run(ctx); err != nil {
        slog.Error("server exited", "err", err)
        os.Exit(1)
    }
}

func must(err error) {
    if err != nil {
        slog.Error("startup failure", "err", err)
        os.Exit(1)
    }
}
```

---

## 12. Scaffolding — Adding a New Event

`scripts/gen-event.sh <domain> [<entity>] <action> <version>` does:

1. Create `schemas/yc-event-schemas/<domain>/<event_type>.json` from a template (e.g. `order/order.placed.v1.json`, `inventory/inventory.stock.reserved.v1.json`; ADR-007)
2. Run `task gen:events` to regenerate Go types
3. Add the event type constant to the appropriate domain package
4. Print a reminder: "update the taxonomy doc, publisher service, and consumer service(s)"

---

## 13. Secrets and Configuration

### Development
- `config/config.default.yaml` — defaults, committed
- `config/config.dev.yaml` — dev overrides, committed (no secrets)
- `.env` — gitignored, holds secrets (DB passwords, API keys)
- Environment variables override YAML (via `yc-config`)

### Production (future scope)
- Secrets from cloud secret manager (AWS Secrets Manager, Vault)
- Config from ConfigMaps or similar
- Standard interface: environment variables, so dev and prod look the same to the service

### `.env.example`

Committed, provides a template for developers:

```bash
# NATS
NATS_URL=nats://localhost:4222

# Postgres (per service)
YC_IDENTITY_DB_URL=postgres://yc_identity_app:devpass@localhost:5432/yc_identity?sslmode=disable
YC_GATEWAY_DB_URL=postgres://yc_gateway_app:devpass@localhost:5432/yc_gateway?sslmode=disable
# ... one per service

# OpenTelemetry
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318

# OAuth (dev). YC_IDENTITY_OAUTH_FAKE=true replaces Google and Facebook
# with a local fake provider (never in production).
YC_IDENTITY_OAUTH_FAKE=true
YC_IDENTITY_GOOGLE_CLIENT_ID=
YC_IDENTITY_GOOGLE_CLIENT_SECRET=
YC_IDENTITY_FACEBOOK_CLIENT_ID=
YC_IDENTITY_FACEBOOK_CLIENT_SECRET=

# Service-to-service client secrets (ADR-021), one per calling service
YC_CART_SERVICE_SECRET=dev-cart-service-secret-32-chars!!

# Payment gateways (dev / test keys)
STRIPE_API_KEY=sk_test_...
YOCO_API_KEY=sk_test_...

# Encryption keys (dev; rotate in prod)
YC_PAYMENT_CIPHER_KEY=dev-32-byte-key-replace-in-prod!
YC_MESSAGE_CIPHER_KEY=dev-32-byte-key-replace-in-prod!
YC_SHIPPING_CIPHER_KEY=dev-32-byte-key-replace-in-prod!
```

---

## 14. Developer Workflow — End to End

A new developer joining the project:

```bash
# 1. Clone
git clone https://github.com/slaghuis/yc-platform.git
cd yc-platform

# 2. Install tools
task check-deps          # verifies Go, Task, Docker, migrate, oapi-codegen, golangci-lint
task bootstrap           # installs pre-commit, downloads deps, generates code

# 3. Start dev stack
task devstack:up

# 4. Run migrations
task migrate:up

# 5. Run a service
task run SERVICE=yc-identity

# 6. In another terminal, run tests
task test
task test:integration

# 7. Make a change, verify
task lint
task test

# 8. Commit
git commit -m "..."       # pre-commit runs fmt, lint, schema checks
```

Adding a feature that spans services:

```bash
# 1. Add an event schema if needed
task scaffold:event DOMAIN=order NAME=order.priority_elevated
task gen:events

# 2. Modify the publishing service
# ... code changes in services/yc-orders ...

# 3. Modify the consuming service(s)
# ... code changes in services/yc-message ...

# 4. Test
task test SERVICE=yc-orders
task test SERVICE=yc-message
task test:e2e

# 5. Commit
```

---

## 15. Required Developer Tools

Documented in `docs/onboarding/developer-setup.md`. Verified by `task check-deps`:

| Tool | Minimum Version | Purpose |
|---|---|---|
| Go | 1.26 | Language |
| Docker | 24+ | Dev stack |
| Docker Compose | V2 | Dev stack |
| Task | 3.30+ | Task runner |
| golang-migrate | 4.17+ | DB migrations |
| golangci-lint | 2.x | Linter |
| gofumpt | latest | Formatter |
| goimports | latest | Import sort |
| oapi-codegen | 2.0+ | OpenAPI generation |
| pre-commit | 3.6+ | Git hooks |
| nats-cli | 0.1+ | NATS debugging |
| psql | 16+ | DB debugging |

---

## 16. Open Decisions

A few points that can be made more concrete when we start implementing. Flagging here so they're not forgotten:

1. **DI style**: hand-wired constructors in `internal/app/wire.go`, or `google/wire`-generated? Recommendation: hand-wired for simplicity; move to `wire` only if composition becomes painful.

2. **HTTP router**: `chi` (lightweight, idiomatic), `gorilla/mux` (mature but slower), or `fiber`/`echo` (frameworks). Recommendation: **chi** — minimal, standard-library-compatible, excellent middleware ecosystem.

3. **Postgres driver**: `pgx/v5` (recommended) vs `database/sql + lib/pq`. Recommendation: **pgx/v5** directly, with `pgxpool` for pooling.

4. **Config library**: `koanf` (powerful), `viper` (popular but opinionated), or a thin custom wrapper. Recommendation: **koanf** — composable, no magic.

5. **Logger**: `slog` (stdlib, Go 1.21+). Recommendation: **slog** with a JSON handler; `yc-telemetry` configures it with trace context.

6. **OpenAPI code generation**: `oapi-codegen` (recommended) vs `ogen` (newer, stricter). Recommendation: **oapi-codegen** for maturity.

7. **Testing containers**: `testcontainers-go`. Yes.

8. **Monorepo CI efficiency**: if build times become painful, consider `nx` or `turborepo` (JS tooling but polyglot) or `bazel` (heavy but powerful). Not needed in v1.
