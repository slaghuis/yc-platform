# Scripts

Helper scripts called from `Taskfile.yml`. Run them through `task` rather than directly.

| Script | Task | Purpose |
|---|---|---|
| `bootstrap.sh` | `task bootstrap` | One-time setup after a fresh clone: pre-commit hooks, `.env`, dev JWT keys |
| `check-deps.sh` | `task check-deps` | Verify required developer tools are installed |
| `gen-dev-jwt-keys.sh` | (called by `bootstrap.sh`) | Generate the RSA keypair for JWT signing in dev, into `deploy/compose/dev-keys/` (gitignored). Not for production |
| `for-each-module.sh` | `task lint`, `task test`, `task tidy` | Run a command in every module listed in `go.work`; reports every failing module |
| `gen-service.sh` | `task scaffold:service NAME=yc-foo` (`NO_DB=1` for a stateless service) | Scaffold a new service and wire it into `go.work`, `postgres-init.sh` and `.env.example` (`docs/onboarding/adding-a-service.md`) |
| `gen-event.sh` | `task scaffold:event DOMAIN=order ACTION=placed VERSION=1` | Scaffold a new event schema; add `ENTITY=...` for non-root entities (`docs/onboarding/adding-an-event.md`) |
| `wait-for-stack.sh` | (called by `task devstack:up`) | Wait for dev stack services to become healthy; fails if any is not ready |
| `oapi-codegen.sh` | `task gen:openapi` (per service) | Generate a service's strict server from `api/openapi.yaml` with its `api/oapi-codegen.yaml` |
| `demo-phase1.sh` … `demo-phase8.sh` | `task demo:phase1` … `demo:phase8` | Milestone demos for each phase of `docs/implementation-plan.md`, run against the dev stack through yc-gateway |
| `load-test.sh` | `task load` | Phase 9 load test: 500 RPS browse, concurrent checkouts and 1000 events/s against the running stack (`docs/reports/load-test.md`) |
| `chaos-test.sh` | (run directly; changes the stack) | Kills or pauses services, NATS and Postgres during checkouts and checks every saga settles and the books reconcile; `./scripts/chaos-test.sh kill-orders` runs one drill (`docs/reports/chaos-test.md`) |
| `lib/stack.sh` | (sourced by the two above) | Shared helpers: checks the stack, points yc-payment at the fake Stripe, provisions shops and catalogs, runs k6 in Docker |

Event schema scripts live with the schemas in `schemas/yc-event-schemas/scripts/` (see `schemas/yc-event-schemas/README.md`).

Scripts must keep LF line endings (enforced by `.gitattributes`) and the executable bit, and must run on the bash 3.2 that ships with macOS (no `${var^^}`, `mapfile` or associative arrays).
