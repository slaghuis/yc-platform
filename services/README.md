# Services

One directory per microservice. Each service is its own Go module.

See `docs/services/` for per-service requirement specifications.

## Common structure

Every service follows the layout in `docs/standards/go-conventions.md`:
cmd/<service>/main.go — entry point internal/ app/ — composition root domain/ — types service/ — use cases repository/ — persistence transport/ http/ — handlers events/ — subscribers api/openapi.yaml — API contract migrations/*.sql — database schema evolution config/config.default.yaml — default config

`
## Scaffolding a new service

```bash
task scaffold:service NAME=yc-foo
`
See `docs/onboarding/adding-a-service.md`
`
---

## Summary of the Scaffold

What you now have, once these files are placed:

### Working out of the box
- `task bootstrap` — one-time setup
- `task devstack:up` — Postgres, NATS, MinIO, Jaeger, MailHog running
- `task test` — all placeholders compile; empty test runs pass
- Pre-commit hooks installed and enforcing format/lint
- CI pipeline configured

### Ready to populate
- 10 shared library modules in `libs/` (empty, but wired into `go.work`)
- 17 service modules in `services/` (placeholder `main.go` each)
- 4 tool modules in `tools/`
- Event schemas directory with envelope + common fragments
- Documentation structure with every file either populated or stubbed

### Standards enforced
- Database per service (`postgres-init.sh` creates all 16)
- Outbox + processed_events tables for every service (migration 001)
- Pre-commit hooks for format, lint, schema validation
- CI for lint, test, schema validation, backward compatibility
- Docker-ready via shared `Dockerfile.service`

### What's deliberately empty
- Shared library implementations (placeholders only; built in Phase 1)
- Service business logic (built per phase in the implementation plan)
- Tool implementations (placeholders; `yc-schema-gen` and `yc-migrate` built in Phase 1)
- ADR content (produced in earlier design sessions — paste into `docs/adr/NNN-*.md`)
- Service spec content (produced earlier — paste into `docs/services/<name>.md`)
- Standards content (produced earlier — paste into `docs/standards/*.md`)

## Three Verification Checks

Once the scaffold is placed, verify:

```bash
# 1. Tooling is in order
task check-deps

# 2. Everything compiles
go work sync
task test

# 3. Dev stack boots
task devstack:up
`

If all three pass, the scaffold is operational.
