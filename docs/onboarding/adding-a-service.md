# Adding a Service

## 1. Scaffold

```bash
task scaffold:service NAME=yc-foo
```

This creates `services/yc-foo/` with the standard directory layout, `go.mod`, `cmd/yc-foo/main.go`, `Taskfile.yml`, migrations for the outbox tables, and a Dockerfile symlink.

## 2. Wire Into the Monorepo

- [ ] Add `./services/yc-foo` to `go.work`
- [ ] Add `yc_foo` to `deploy/compose/postgres-init.sh`
- [ ] Add `YC_FOO_DB_URL` to `.env.example`
- [ ] Add `yc-foo` to CI matrices and workflows if needed
- [ ] Create `docs/services/yc-foo.md` with the full requirement spec

## 3. Implement

Follow the structure in `docs/standards/go-conventions.md`:

```
services/yc-foo/
├── cmd/yc-foo/main.go           — entry point
├── internal/
│   ├── app/                      — composition root
│   ├── domain/                   — types, no I/O
│   ├── service/                  — use cases
│   ├── repository/               — persistence
│   └── transport/
│       ├── http/                 — handlers
│       └── events/               — subscribers
└── migrations/                   — schema evolution
```

Reference the yc-identity service as a worked example.

## 4. Expose Routes

Register with yc-gateway at startup (or add to the static route file for cold-start availability).

## 5. Test

- Unit tests next to source
- Integration tests under `tests/integration/` with build tag `integration`
- Register the service in `test/e2e/` if it participates in cross-service flows
