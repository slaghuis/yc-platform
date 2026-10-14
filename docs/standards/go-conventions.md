# Go Conventions

Code style and structural conventions for all Go code in yc-platform. These rules supplement the standard Go idioms documented at [Effective Go](https://go.dev/doc/effective_go) and [Google's Go Style Guide](https://google.github.io/styleguide/go/).

When in doubt, prefer the stricter interpretation. Consistency across services matters more than individual preferences.

## 1. Formatting and Tooling

- **gofumpt** is mandatory; `gofmt` is insufficient.
- **goimports** must run with the local prefix: `-local github.com/<org>/yc-platform`.
- **golangci-lint** must pass with no warnings on every PR.
- Pre-commit hooks run these automatically; CI blocks merge if they haven't been run.

## 2. Package Design

- **One concept per package.** A package exports a coherent set of types and functions around one responsibility.
- **No `util`, `common`, `helpers`, or `shared` packages.** These are code smells. Find a meaningful name or put the code where it belongs.
- **Package names are singular, lowercase, no underscores.** `user`, not `users` or `user_service`.
- **Package names match the directory.** The compiler enforces this; we also forbid using `package foo` in a directory named `bar`.
- **The `internal/` boundary is sacred.** A service's internals are not accessible to other services, even within the same monorepo. Shared code lives in `/libs/`.

### Standard package layout within a service

```
internal/
├── app/           — composition root, config, DI wiring
├── domain/        — pure types and errors; no I/O, no dependencies on infrastructure
├── service/       — use cases; orchestrates domain + repositories + external calls
├── repository/    — persistence; one file per aggregate
├── transport/
│   ├── http/      — HTTP handlers, routers, middleware
│   └── events/    — event subscribers
└── integration/   — external service clients (gateways, OAuth providers, etc.)
```

Code flows **inward**: `transport` → `service` → `domain` and `repository`. Domain never imports transport. Service never imports transport.

## 3. Naming

### Variables and functions

- **Short names for short scopes, descriptive names for long scopes.** A loop counter is `i`; a function parameter used throughout a 100-line function is not.
- **Avoid stutter.** `user.User` is bad; `user.Account` or `user.Record` is better. `user.NewUser()` is `user.New()`.
- **Receivers**: single lowercase letter matching the type. `func (u *User) ...`, not `func (self *User) ...` or `func (this *User) ...`.
- **Interface names**: often `-er` suffix for single-method interfaces (`Reader`, `Validator`), but prefer a descriptive name when the role is complex (`UserRepository`, `PaymentGateway`).

### Files

- **Lowercase, underscore-separated.** `user_repository.go`, `oauth_google.go`.
- **One top-level type per file** when the type is significant. Smaller types can share a file.
- **Test files** mirror the source: `user_repository.go` → `user_repository_test.go`.

### Constants

- **PascalCase for exported, camelCase for unexported.** No `SCREAMING_SNAKE_CASE`.
- **Group related constants** in a `const` block.
- **Use typed constants** when they represent a domain value: `type Role string; const RoleAdmin Role = "admin"`.

### Errors

- **Sentinel errors** are exported variables prefixed with `Err`: `ErrNotFound`, `ErrConflict`.
- **Error types** are exported structs suffixed with `Error`: `ValidationError`, `UpstreamError`.
- See `error-handling.md` for the full taxonomy.

## 4. Imports

Three groups, separated by blank lines, in this order:

```go
import (
    "context"
    "fmt"
    "net/http"

    "github.com/go-chi/chi/v5"
    "github.com/jackc/pgx/v5"

    "github.com/<org>/yc-platform/libs/yc-authctx"
    "github.com/<org>/yc-platform/libs/yc-errors"
    "github.com/<org>/yc-platform/services/yc-identity/internal/domain"
)
```

1. Standard library
2. Third-party
3. Local (anything under `github.com/<org>/yc-platform/`)

`goimports -local github.com/<org>/yc-platform` enforces this.

## 5. Contexts

- **Always accept `context.Context` as the first parameter** on any function that does I/O, blocks, or spans goroutines.
- **Never store a context in a struct.** Pass it explicitly.
- **Never pass `context.TODO()` in new code.** Use `context.Background()` only at process entry (`main`); use the caller's context everywhere else.
- **Never pass `nil` for a context.** This panics in many standard library functions.
- **Honor cancellation.** Check `ctx.Err()` in long loops; pass `ctx` to all I/O calls.
- **Context values are for request-scoped data only**, not for configuration or dependencies.
- **Use typed keys** for context values. Never use string keys.

## 6. Error Handling

- **Return errors as the last value.** Never return `(error, Result)`.
- **Check every error.** Linter enforces this; the only exceptions are `defer`red cleanup (and even then, log on failure).
- **Wrap errors with context** using `fmt.Errorf("...: %w", err)` or `errors.Wrap`.
- **Do not log and return.** Pick one. Logging is the responsibility of the handler at the top of the stack; propagation is the responsibility of everything else.
- See `error-handling.md` for details.

## 7. Nil

- **Return zero values and `nil` for errors**, not sentinel objects. A nil pointer with a nil error is a bug.
- **Methods on nil pointers** are acceptable when they're defensive (e.g., `(*Logger).Debug` on a nil `Logger` is a no-op). Document when you do this.
- **Avoid nil maps and nil slices in exported return values** when callers will iterate; return empty collections instead.

## 8. Concurrency

- **Share by communicating, not by sharing memory.** Prefer channels and goroutines over locks when the problem naturally fits.
- **Use `sync.Mutex` when protecting shared state is simpler** than restructuring around channels. There is no prize for forcing channels.
- **Every goroutine must have an owner responsible for lifecycle.** Goroutines without a shutdown path are leaks.
- **`errgroup` for coordinated goroutines** that can fail together.
- **No `go func() { ... }()` in handlers.** Spawn via a managed pool or `errgroup`; always propagate the request context.
- **Mutex field placement**: immediately above the fields it protects, documented inline.

```go
type Cache struct {
    mu    sync.RWMutex
    items map[string]Entry
}
```

## 9. Interfaces

- **Accept interfaces, return structs.** Define interfaces at the consumer, not the producer.
- **Keep interfaces small.** Prefer a 1–3 method interface over a 10-method one. Composition over inheritance.
- **Interfaces for seams, not for ceremony.** Don't define an interface for every type "just in case." Add it when you need test substitution or a second implementation.

Good:

```go
// In the service package, where we need to abstract persistence
type UserRepository interface {
    Find(ctx context.Context, id string) (*domain.User, error)
    Save(ctx context.Context, u *domain.User) error
}

// In the repository package, we return a concrete struct
func NewPostgresUserRepository(db *pgxpool.Pool) *PostgresUserRepository { ... }
```

## 10. Structs

- **Field order**: group logically; place critical identifiers first, metadata last.
- **Zero values should be usable** where possible. Avoid `MustInit` patterns.
- **Struct tags** go on their own line only if long; otherwise inline.
- **No embedding unless there's a semantic "is-a" relationship.** Embedding is composition shorthand, not inheritance.

## 11. Generics

- **Use when it genuinely reduces duplication**, especially for container types and functional helpers.
- **Avoid generic abstractions over business logic.** A generic `Service[T any]` is almost always a mistake.
- **Prefer concrete over generic** when the number of instantiations is 1 or 2.

## 12. Dependencies

- **Prefer the standard library.** Add a dependency only when it provides significant value.
- **Approved foundational dependencies** (require no justification):
  - `github.com/go-chi/chi/v5` — HTTP routing
  - `github.com/jackc/pgx/v5` — Postgres
  - `github.com/nats-io/nats.go` — NATS
  - `github.com/golang-migrate/migrate/v4` — migrations
  - `github.com/knadh/koanf/v2` — config
  - `github.com/google/uuid` — UUID
  - `go.opentelemetry.io/otel/*` — observability
  - `github.com/stretchr/testify` — testing
  - `github.com/testcontainers/testcontainers-go` — integration testing
- **New dependencies require an ADR** or a reviewer-approved justification in the PR.
- **No unmaintained or single-maintainer-abandoned packages.** Check last commit date and open issue trends.

## 13. Logging

- **Use `slog` via `yc-telemetry`.** Never use `log`, `fmt.Println`, or third-party loggers directly.
- **Structured logging only.** No format strings with embedded values.
- See `logging-tracing.md`.

## 14. Comments

- **Exported identifiers must have a comment** starting with the identifier name.
- **Comments explain _why_, not _what_.** The code already says what.
- **TODO comments must reference an issue or an ADR.** `// TODO(ADR-012): once backorders are supported, ...`
- **No commented-out code.** Delete it; it lives in git history.

## 15. Testing

- Test files alongside source files.
- Table-driven tests for anything with multiple cases.
- Use `t.Parallel()` where tests are safe to parallelize.
- See `testing.md`.

## 16. File Length

- **Soft limit: 500 lines per file.** Larger means the file probably has multiple responsibilities.
- **Hard limit: 1500 lines.** Beyond this, split before merging.
- Generated files are exempt.

## 17. Function Length

- **Soft limit: 50 lines.** Longer functions are usually candidates for extraction.
- **Hard limit: 150 lines** (excluding comments and declarations).
- Deeply nested conditionals (more than 3 levels) are a strong signal to extract.

## 18. Prohibited Patterns

- **`panic` outside `main` or `init`.** All library and service code must return errors.
- **`init()` functions** except for registering with a package-level registry (and even then, prefer explicit registration).
- **Global mutable state.** Package-level variables must be constants or `sync.Once`-initialized read-only configuration.
- **`interface{}` / `any` in function signatures** unless there's no alternative. Use generics or concrete types.
- **Reflection** unless deserializing, generic container implementation, or similar genuinely-needed use.
- **`time.Now()` called directly in business logic.** Inject a clock. Makes tests deterministic.

## 19. The Clock

Every service injects a clock:

```go
type Clock interface {
    Now() time.Time
}

type RealClock struct{}
func (RealClock) Now() time.Time { return time.Now().UTC() }
```

- All timestamps are UTC.
- Tests use `FakeClock` from `yc-testkit`.

## 20. UUIDs

- `github.com/google/uuid` for generation.
- UUIDv7 preferred (time-ordered, index-friendly) once we settle on a stable library version. v4 is acceptable in the interim.
- Stored as Postgres `UUID` type, not text.
