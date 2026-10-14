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

Event schema scripts live with the schemas in `schemas/yc-event-schemas/scripts/` (see `schemas/yc-event-schemas/README.md`).

Scripts must keep LF line endings (enforced by `.gitattributes`) and the executable bit, and must run on the bash 3.2 that ships with macOS (no `${var^^}`, `mapfile` or associative arrays).
