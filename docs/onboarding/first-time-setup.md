# First-Time Setup

Install the tools in [`developer-setup.md`](developer-setup.md) first.

```bash
git clone https://github.com/slaghuis/yc-platform.git
cd yc-platform

task bootstrap        # installs hooks, creates .env, generates dev keys
task devstack:up      # starts Postgres, NATS, MinIO, Jaeger, MailHog
task test             # verify everything compiles and tests pass
```

## Local Services

Once the dev stack is up:

| Service | URL |
|---|---|
| Postgres | `postgres://postgres:devpass@localhost:5432` |
| NATS | `nats://localhost:4222` |
| NATS admin | http://localhost:8222 |
| MinIO | http://localhost:9001 (devkey / devsecret) |
| Jaeger | http://localhost:16686 |
| MailHog | http://localhost:8025 |

## Troubleshooting

**Port conflicts.** If port 5432 (or any other) is in use, remap it in `docker-compose.override.yml` at the repo root; the file contains commented examples.

**Dev stack won't start.**

```bash
task devstack:reset   # wipes volumes and restarts
```

**Pre-commit failures.**

```bash
pre-commit run --all-files   # see what's failing
```

**Go workspace issues.** The repo is a Go workspace (`go.work`) with one module per lib, service and tool, and no root `go.mod`.
- Relative wildcards that span modules fail from the root (`go test ./libs/...` reports `directory prefix . does not contain modules listed in go.work`). Use `task test`, run from inside a module directory, or use the module path (`go test github.com/slaghuis/yc-platform/libs/yc-eventbus/...`).
- After adding a module, add it to `go.work` (`go work use ./services/yc-foo`).
- If dependency versions disagree between modules, run `go work sync`.
