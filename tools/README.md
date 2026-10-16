# Tools

Developer and operational utilities.

| Tool | Purpose | Status |
|---|---|---|
| `yc-schema-gen`  | Generate Go types in `libs/yc-events` from the event JSON Schemas (`task gen:events`) | Built |
| `yc-migrate`     | Run golang-migrate across services | Placeholder (so `task migrate:up` does nothing); services apply their own migrations at start-up when `database.migrate_on_start` is set |
| `yc-dlq-cli`     | `list`, `show` and `replay` dead letters from the `YC_DLQ` stream (`runbooks/dlq-replay.md`) | Built in Phase 8 |
| `yc-tenant-cli`  | `provision` calls `POST /sysadmin/tenants` with `YC_TENANT_CLI_TOKEN` (`runbooks/onboarding-tenant.md`) | Built; `verify` is in `backlog-v2.md` |

Each tool is a standalone Go module. Run via `go run ./tools/<name>` from the
repo root.
