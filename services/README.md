# Services

One directory per microservice. Each service is its own Go module, listed in the root `go.work`.

See `docs/services/` for the per-service requirement specifications.

## Common structure

Every service follows the layout in `docs/standards/go-conventions.md`:

```
services/<service>/
├── cmd/<service>/main.go        — entry point
├── internal/
│   ├── app/                     — composition root
│   ├── domain/                  — types
│   ├── service/                 — use cases
│   ├── repository/              — persistence
│   └── transport/
│       ├── http/                — handlers
│       └── events/              — subscribers
├── api/openapi.yaml             — API contract
├── migrations/*.sql             — database schema evolution
└── config/config.default.yaml   — default config
```

The current placeholders still have `main.go` at the service root; they move to `cmd/<service>/` when each service is implemented. The pipeline builds an image only for services with a `cmd/<service>` directory (ADR-018).

## Scaffolding a new service

```bash
task scaffold:service NAME=yc-foo
```

See `docs/onboarding/adding-a-service.md`.
