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

## Application Services

`task devstack:app` builds and starts the application services in Docker (compose profile `app`). `task demo:phase1` runs the Phase 1 milestone demo and the outbox and DLQ drills (`scripts/demo-phase1.sh milestone|outbox|dlq` runs one part); `task demo:phase2` runs the catalog-and-browse milestone through the gateway; `task demo:phase3` runs the cart milestone (visitor cart, registration, fake Google login and merge, display currency, price change, popularity search, rate limits). Product, pricing, image, preference and cart register their routes with the gateway on start-up, so give them a few seconds after `readyz`.

| Service | URL |
|---|---|
| yc-gateway | http://localhost:8090 — send `Host: demo-shop.localhost` after a demo provisions `demo-shop`, or `Host: admin.localhost` (the platform host: system admins only) |
| yc-identity | http://localhost:8081 |
| yc-monitor | http://localhost:8082 — `/sysadmin/*` needs `X-YC-Tenant-ID: _platform` and `X-YC-Actor-Type: system_admin` headers |
| yc-product | http://localhost:8083 |
| yc-pricing | http://localhost:8084 — exchange rates come from api.frankfurter.dev, so display currencies need internet access |
| yc-image | http://localhost:8085 — image URLs point at the S3 bucket on http://localhost:9000 |
| yc-preference | http://localhost:8086 |
| yc-cart | http://localhost:8087 — prices carts through the gateway's internal listener (`YC_CART_SERVICE_SECRET`) |

The first system admin is created with `docker exec -e YC_BOOTSTRAP_ADMIN_PASSWORD=… yc-identity /app/service bootstrap-admin --email you@example.com` (or `task run SERVICE=yc-identity -- bootstrap-admin …` locally); log in on `admin.localhost`. Shop admins register on their shop as customers and a system admin promotes them (`POST /admin/users/{id}/promote` on the admin host).

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
