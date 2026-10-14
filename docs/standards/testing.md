# Testing

Testing conventions for yc-platform. Tests are first-class deliverables; untested code does not merge.

## 1. Test Pyramid

| Layer | Scope | Speed | Count |
|---|---|---|---|
| Unit | Single function or type | <10ms | Many |
| Integration | Service with real DB, NATS | 100ms–1s | Several per service |
| End-to-end | Multiple services, full saga | 5–30s | A few, cross-cutting |

Target distribution: 70% unit, 25% integration, 5% e2e. Measured not by test count but by signal value.

## 2. Test Location

- **Unit tests**: alongside source files in the same package. `foo.go` has `foo_test.go` with `package foo`.
- **Internal unit tests**: for testing unexported functions. Same package.
- **External unit tests**: for testing the public API. Suffix `_test` on the package. Use when you want to catch accidental exports or verify the public API shape.
- **Integration tests**: in the service repo under `tests/integration/`. Build tag: `//go:build integration`.
- **End-to-end tests**: in `test/e2e/` at the repo root. Build tag: `//go:build e2e`.

## 3. Running Tests

```bash
# Unit tests only (default)
go test ./...
task test

# Integration tests (requires devstack)
go test -tags=integration ./...
task test:integration

# E2E tests (requires full stack)
go test -tags=e2e ./test/e2e/...
task test:e2e
```

Unit tests must run **without any external dependencies**. No Docker, no network, no filesystem beyond temp dirs. CI runs them first; they must be fast.

## 4. Test Framework

- **Standard library `testing` package.** No alternative frameworks.
- **`github.com/stretchr/testify`** for assertions. `require` for fail-fast, `assert` for continue-on-failure.
- **No DSLs** (Ginkgo, Gomega). We use plain Go.

```go
import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestFoo(t *testing.T) {
    t.Parallel()
    result, err := Foo()
    require.NoError(t, err)
    assert.Equal(t, "expected", result.Value)
}
```

## 5. Table-Driven Tests

For any function with multiple cases, use a table:

```go
func TestEmailValidation(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        wantErr bool
    }{
        {"valid", "user@example.com", false},
        {"missing at", "userexample.com", true},
        {"empty", "", true},
        {"unicode local", "yöü@example.com", false},
    }

    for _, tt := range tests {
        tt := tt
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            err := ValidateEmail(tt.input)
            if tt.wantErr {
                require.Error(t, err)
            } else {
                require.NoError(t, err)
            }
        })
    }
}
```

- **Capture the loop variable** (`tt := tt`) before `t.Parallel()`.
- **Name every case**; names appear in test output.
- **One assertion per case** when possible.

## 6. Parallelism

- **Call `t.Parallel()`** in every unit test unless there's a reason not to.
- **Integration tests parallelize carefully**: each test uses its own database and NATS subject namespace.
- **No shared mutable state** between tests. If tests share setup, build setup helpers that create isolated instances.

## 7. Test Doubles

- **Fakes over mocks.** A hand-written in-memory implementation of an interface is clearer than a mock.
- **Mocks when you must assert on specific calls.** We don't use mock-generation libraries; hand-write them.
- **Stubs for simple return-value injection.**

```go
// In-memory fake repository for testing the service layer
type FakeUserRepo struct {
    users map[string]*domain.User
}

func (f *FakeUserRepo) Find(ctx context.Context, id string) (*domain.User, error) {
    u, ok := f.users[id]
    if !ok {
        return nil, repository.ErrUserNotFound
    }
    return u, nil
}
```

Fakes live in `_test.go` files unless they're shared across multiple test files, in which case they go in `testing_support.go` within the package.

## 8. Test Data

- **Builders** for complex types. Make the common case cheap, the custom case explicit:

```go
func NewTestUser(opts ...UserOption) *domain.User {
    u := &domain.User{
        ID:       "user-" + uuid.NewString(),
        Email:    "test@example.com",
        TenantID: "test-tenant",
        Role:     "customer",
    }
    for _, opt := range opts {
        opt(u)
    }
    return u
}

type UserOption func(*domain.User)

func WithEmail(e string) UserOption { return func(u *domain.User) { u.Email = e } }
func WithRole(r string) UserOption  { return func(u *domain.User) { u.Role = r } }
```

- **Fixtures** for complex JSON or SQL go under `tests/testdata/`.
- **No reliance on specific IDs** unless the test is explicitly about ID handling. Use random UUIDs.

## 9. Clock

- **Never call `time.Now()` in production code** (see Go Conventions §19).
- **Tests use `FakeClock` from `yc-testkit`**:

```go
clock := testkit.NewFakeClock("2025-01-15T10:00:00Z")
svc := service.New(repo, clock)

// Advance clock explicitly in tests
clock.Advance(5 * time.Minute)
```

## 10. Context in Tests

Pass a real context — usually `t.Context()` (Go 1.24+) or `context.Background()`:

```go
func TestFoo(t *testing.T) {
    ctx := context.Background()
    result, err := Foo(ctx)
    ...
}
```

For tests that need auth context, use `yc-testkit`:

```go
ctx := testkit.ContextWithAuth(ctx, testkit.AuthContext{
    TenantID:  "tenant-abc",
    ActorID:   "user-123",
    ActorType: "customer",
})
```

## 11. Integration Tests

Use `testcontainers-go` via `yc-testkit`:

```go
//go:build integration

package integration

import (
    "testing"

    "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

func TestRegistration(t *testing.T) {
    ctx := context.Background()
    db := testkit.StartPostgres(t)          // auto-cleanup via t.Cleanup
    bus := testkit.StartNATS(t)
    svc := app.New(cfg, db, bus)

    ...
}
```

- **One container per test** by default. The helper caches containers across tests in the same binary when safe (connection pooling) but gives each test an isolated database.
- **Migrations run automatically** at container start.
- **No `TestMain` setup/teardown** unless truly necessary; prefer `t.Cleanup`.
- **No shared state between tests.**

## 12. End-to-End Tests

Live under `test/e2e/`. These:
- Start the full docker-compose stack (or assume it's running).
- Exercise the real HTTP APIs through yc-gateway.
- Verify observable outcomes (DB state, emitted events, downstream effects).

```go
//go:build e2e

func TestCheckoutHappyPath(t *testing.T) {
    client := testkit.NewE2EClient("http://localhost:8080")

    tenant := testkit.ProvisionTenant(t)
    seedCatalog(t, tenant)

    customer := client.RegisterCustomer(t, "customer@example.com", "...")
    client.AuthenticateAs(customer)

    client.AddToCart(t, skuID, 2)
    session := client.StartCheckout(t)
    client.ConfirmCheckout(t, session.ID, testkit.StripeTestToken())

    testkit.EventuallyWithT(t, 10*time.Second, func(c *testkit.CollectT) {
        order := client.GetLatestOrder(t)
        c.Equal("received", order.Status)
    })
}
```

- **Prefer polling with timeout** over fixed sleeps.
- **Keep E2E count low**: one or two tests per saga variant.
- **E2E tests are the slowest and flakiest**; failures must get fixed, not ignored.

## 13. Snapshot Tests

For complex structured output (pricing results, order snapshots), snapshot tests with golden files:

```go
func TestPricingResult(t *testing.T) {
    result := Evaluate(testContext)
    testkit.AssertGolden(t, "pricing_vat_inclusive.json", result)
}
```

- **Golden files live alongside tests** in `testdata/`.
- **Update with `-update` flag**: `go test -run TestPricingResult -update`.
- **Review golden diffs carefully** in PRs; a changed golden is a changed contract.

## 14. Coverage

- **No strict coverage threshold enforced** in CI.
- **Target 80% for business logic** (services, domain, pricing evaluators).
- **No coverage target for transport or wiring**; these are better tested by integration tests.
- Use `go test -cover` locally; CI publishes coverage reports but does not fail on numbers.

## 15. Flaky Tests

- A flaky test is a bug. Fix or delete.
- **Never add `t.Skip` as a workaround** without a linked issue and a deadline.
- **`EventuallyWithT`** with explicit timeout for async assertions, not `time.Sleep`.
- **No tests that depend on wall-clock time** unless using `FakeClock`.
- **Random inputs** (fuzz-adjacent) should be deterministic in CI (seeded).

## 16. Fuzzing

- Use Go's built-in fuzzing (`go test -fuzz`) for parsers, deserializers, and anything taking untrusted input.
- Fuzz corpus committed under `testdata/fuzz/`.
- Run on PRs that touch fuzz targets; a crash is a merge blocker.

## 17. Benchmark Tests

- Benchmarks for hot paths (pricing evaluation, event bus throughput, DB query performance).
- Named `BenchmarkFoo`; run with `go test -bench`.
- Benchmarks are not run in CI; they are tools for performance work.

## 18. Testing Internals vs Public API

- **Default to testing through the public API.** More resilient to refactoring.
- **Test internals when they contain non-trivial logic** that's awkward to exercise through the API.
- **Private helpers can be tested directly** in `_test.go` files in the same package.

## 19. Test Hygiene

- **No `fmt.Println` in tests.** Use `t.Log` for debug output.
- **No `panic` or `os.Exit` in tests.** Use `t.Fatal`.
- **Clean up after yourself.** Use `t.Cleanup` for cleanup; no reliance on `TestMain`.
- **Delete tests for deleted features.** Don't leave zombie tests "just in case."
- **Test names describe behavior**, not implementation: `TestRegister_FailsWhenEmailExists`, not `TestRegisterCallsRepoFind`.

## 20. The Review Standard

In a PR, reviewers check:
- Every new function has at least one test.
- Every bug fix has a regression test.
- Every new event type has a producer test and a consumer test.
- Every new API endpoint has a success test and at least one failure test.
- No `t.Skip` without justification.
- No `time.Sleep` in tests except where genuinely necessary (and justified in a comment).

---
