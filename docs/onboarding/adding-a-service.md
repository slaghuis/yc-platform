# Adding a Service

## 1. Scaffold

```bash
task scaffold:service NAME=yc-foo            # with a database
task scaffold:service NAME=yc-foo NO_DB=1    # stateless service
```

This creates `services/yc-foo/` with `go.mod`, `cmd/yc-foo/main.go` (logs "hello", stops cleanly on SIGTERM), `Taskfile.yml`, `config/config.default.yaml`, `README.md`, and the eventbus migrations (copied from `libs/yc-eventbus/schema.sql`). It also:

- adds `./services/yc-foo` to `go.work`;
- adds the `yc_foo` database to `deploy/compose/postgres-init.sh` (run `task devstack:reset` to create it);
- adds `YC_FOO_DB_URL` to `.env.example` (copy the line into your `.env`).

`task run SERVICE=yc-foo` then builds and runs it. Root tasks (`task build`, `task test`, `task lint`) pick the service up automatically.

## 2. Finish Wiring

- [ ] Create `docs/services/yc-foo.md` with the full requirement spec
- [ ] Add `yc-foo` to the image matrix in `.github/workflows/release.yml`

## 3. Implement

Follow the structure in `docs/standards/go-conventions.md`:

The scaffold creates only the files listed above; add these directories as the service grows:

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
