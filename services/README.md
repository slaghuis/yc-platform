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

Each service starts from the scaffold: `cmd/<service>/main.go`, `Taskfile.yml`, `config/config.default.yaml` and, for services with a database, the eventbus migrations. The other directories are added as the service is implemented. The pipeline and `task build` build every service that has a `cmd/<service>` directory (ADR-018), which is currently all of them.

## Scaffolding a new service

```bash
task scaffold:service NAME=yc-foo
```

See `docs/onboarding/adding-a-service.md`.
