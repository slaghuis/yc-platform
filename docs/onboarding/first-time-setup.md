# First-Time Setup

Install the tools in [`developer-setup.md`](developer-setup.md) first.

```bash
git clone https://github.com/slaghuis/yc-platform.git
cd yc-platform

task bootstrap        # installs hooks, creates .env, generates dev keys
task devstack:up      # starts Postgres, NATS, S3 (SeaweedFS), Jaeger, MailHog
task test             # verify everything compiles and tests pass
```

## Local Services

Once the dev stack is up:

| Service | URL |
|---|---|
| Postgres | `postgres://postgres:devpass@localhost:5432` |
| NATS | `nats://localhost:4222` |
| NATS admin | http://localhost:8222 |
| S3 API (SeaweedFS) | http://localhost:9000 (devkey / devsecret, bucket `yc-images`) |
| S3 admin UI | http://localhost:9001 |
| Jaeger | http://localhost:16686 |
| MailHog | http://localhost:8025 |

## Phase 1 Services

`task devstack:app` builds and starts yc-identity, yc-gateway and yc-monitor in Docker (compose profile `app`); `task demo:phase1` then runs the milestone demo and the outbox and DLQ drills against them (`scripts/demo-phase1.sh milestone|outbox|dlq` runs one part).

| Service | URL |
|---|---|
| yc-gateway | http://localhost:8090 — send `Host: demo-shop.localhost` after the demo provisions `demo-shop` |
| yc-identity | http://localhost:8081 |
| yc-monitor | http://localhost:8082 — `/sysadmin/*` needs `X-YC-Tenant-ID: _platform` and `X-YC-Actor-Type: system_admin` headers |

`task run SERVICE=…` uses the same host ports (set in `.env`). Stop the containers first, or they hold the ports.

## Troubleshooting

**Port conflicts.** If port 5432 (or any other) is in use, copy `docker-compose.override.yml.example` to `docker-compose.override.yml` (gitignored) and remap the port there; the example shows how.

**`database "yc_gateway" does not exist`.** `postgres-init.sh` only runs on an empty volume, so a volume created before a service's database was added lacks it. Run `task devstack:reset`, or create just that database with the statements from `deploy/compose/postgres-init.sh`.

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
