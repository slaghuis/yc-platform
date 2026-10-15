# Error Handling

How errors are created, wrapped, propagated, logged, and returned to clients across yc-platform.

## 1. Principles

1. **Errors are values**, handled explicitly. Never swallow, never ignore.
2. **Context accumulates as errors propagate.** Wrap when you add information; return bare when you don't.
3. **Log at the top, propagate everywhere else.** Transport-layer handlers log; everything else returns.
4. **Classify errors by category**, not by package. Clients and operators care about "was this my fault?" not "which internal module?".
5. **Separate operational errors from programming errors.** Panics are for the latter.

## 2. Error Taxonomy

All errors in `yc-errors` have a stable `Code` and `Status`. The code is the identifier clients and operators use.

### Operational error codes

| Code | HTTP Status | Meaning |
|---|---|---|
| `not_found` | 404 | Resource doesn't exist within the actor's scope |
| `conflict` | 409 | Resource state prevents the operation (duplicate key, invalid transition) |
| `validation_failed` | 400 | Input failed validation; field-level errors available |
| `unauthorized` | 401 | Missing or invalid credentials |
| `forbidden` | 403 | Authenticated but not permitted |
| `gone` | 410 | Resource existed, now deleted; will not return |
| `precondition_failed` | 412 | Optimistic concurrency check failed |
| `rate_limited` | 429 | Too many requests |
| `upstream_failure` | 502 | External service failed |
| `upstream_timeout` | 504 | External service timed out |
| `unavailable` | 503 | Service deliberately unavailable (maintenance, tenant suspended) |
| `internal` | 500 | Unexpected server failure |

### Semantic error codes (status 400 or 409 depending)

| Code | Meaning |
|---|---|
| `insufficient_stock` | Inventory shortage |
| `price_quote_expired` | Checkout session TTL exceeded |
| `payment_declined` | Gateway declined the payment |
| `gateway_unavailable` | Payment gateway not reachable |
| `tenant_suspended` | Operations blocked for the tenant |
| `invalid_state_transition` | e.g., cancelling an already-shipped order |

New codes are added by PR; the list is tracked in `libs/yc-errors/codes.go`.

## 3. The `Error` Type

```go
package errors

type Error struct {
    Code      string
    Status    int
    Message   string         // user-safe
    Internal  string         // operator-only, logged, never returned
    Fields    []FieldError   // for validation errors
    Cause     error          // for wrapping
    Extensions map[string]any // extra problem members, user-safe
}

type FieldError struct {
    Field   string
    Code    string
    Message string
}
```

- `Code` identifies the kind of error.
- `Status` maps to HTTP response status.
- `Message` is safe to return to the client.
- `Internal` carries operator-only context, logged but never returned.
- `Fields` for structured validation errors.
- `Cause` is the wrapped underlying error (optional).
- `Extensions` are extra top-level members of the problem body (RFC 9457 §3.2) for data a client acts on, set with `.With(key, value)`. For example, yc-inventory's `insufficient_stock` carries the `shortages` a storefront shows the customer:

  ```go
  return nil, ycerrors.New(ycerrors.CodeInsufficientStock, "Some items are not available in the quantity requested").
      With("shortages", shortages)
  ```

  They are returned to the client, so they must be user-safe. The standard member names (`type`, `title`, `status`, `detail`, `code`, `correlation_id`, `errors`) are reserved. `Problem` reads unknown members back into `Extensions`, so a calling service (`httpkit.UpstreamProblem`) can act on them too.

## 4. Creating Errors

Prefer the predefined constructors:

```go
return nil, errors.NotFound("user", id)
return nil, errors.Conflict("email already registered")
return nil, errors.Validation().
    Field("email", "required", "Email is required").
    Field("age", "out_of_range", "Must be at least 18").
    Build()
return nil, errors.Forbidden("role_required: shop_admin")
```

Low-level packages return sentinel errors that higher layers translate:

```go
// In repository
var ErrUserNotFound = errors.New("user not found")

// In service
user, err := repo.Find(ctx, id)
if err != nil {
    if errors.Is(err, repository.ErrUserNotFound) {
        return nil, yc_errors.NotFound("user", id)
    }
    return nil, yc_errors.Internal("find user").Wrap(err)
}
```

## 5. Wrapping

Wrap when you add context:

```go
if err := p.Charge(ctx, req); err != nil {
    return fmt.Errorf("yoco charge for session %s: %w", sessionID, err)
}
```

Do not wrap when you'd only add the function name:

```go
// Bad — adds no information
if err := repo.Save(ctx, u); err != nil {
    return fmt.Errorf("save: %w", err)
}

// Good — the stack frame already tells you where
return repo.Save(ctx, u)
```

## 6. Unwrapping and Checking

Use `errors.Is` for sentinel comparisons:

```go
if errors.Is(err, repository.ErrNotFound) { ... }
```

Use `errors.As` for extracting typed errors:

```go
var ve *yc_errors.Error
if errors.As(err, &ve) && ve.Code == "validation_failed" { ... }
```

## 7. The HTTP Boundary

The HTTP transport layer is the **only** place that converts errors to responses. It uses `yc-errors.WriteError`:

```go
func (h *UserHandler) Register(w http.ResponseWriter, r *http.Request) {
    var req RegisterRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        yc_errors.WriteError(w, r, yc_errors.Validation().Message("invalid JSON").Wrap(err))
        return
    }

    user, err := h.svc.Register(r.Context(), req)
    if err != nil {
        yc_errors.WriteError(w, r, err)
        return
    }

    httpkit.WriteJSON(w, http.StatusCreated, user)
}
```

`WriteError`:
1. Extracts the `*Error` via `errors.As`.
2. Writes the response with the error's status and problem+JSON body.
3. Logs the full chain including `Internal` and `Cause` with correlation ID.
4. For unclassified errors, logs them and returns a 500 `internal` to the client.

**Clients never see the internal error chain.** Only `Code`, `Message`, and `Fields`.

## 8. Logging Errors

- **Log once, at the handler boundary.** Not at every layer that passes an error up.
- **Use `slog.Error`** with the error attached:
  ```go
  logger.Error("checkout failed", slog.Any("err", err), slog.String("session_id", sessionID))
  ```
- **Internal classification**: 5xx errors always log at `ERROR`; 4xx errors log at `INFO` or `WARN` depending on frequency and seriousness. A 404 is not an error worth paging anyone over.
- **Error details**: `yc-telemetry` extracts the full chain, `Internal`, and `Cause` and formats them in the log attributes.

## 9. Panics

- **Never panic in library or service code.**
- **The only exceptions**:
  - `main` on startup failure (misconfigured service — fail loudly, fail fast).
  - Impossible conditions where recovery is nonsensical (e.g., `math.MaxInt + 1` in a signed context — but prefer to return an error).
- **Recover from panics in transport-layer middleware**: `yc-httpkit` includes a recover middleware that logs the panic with stack trace and returns 500. This is defense in depth; code should not panic.

## 10. Context Errors

- `context.Canceled` and `context.DeadlineExceeded` are not application errors.
- Services check `ctx.Err()` at loop boundaries; return the context error directly.
- Transport layer maps `context.DeadlineExceeded` to 504 `upstream_timeout` when the deadline is upstream-set; otherwise it's a client disconnect, log at INFO.

## 11. Validation Errors

- **Validate at the edges.** Transport layer validates request shape; service layer validates business rules.
- **Return structured field errors** so UIs can render per-field messages.

```go
v := errors.Validation()
if req.Email == "" {
    v = v.Field("email", "required", "Email is required")
}
if !isValidPhoneE164(req.Phone) {
    v = v.Field("phone", "format", "Phone must be in E.164 format")
}
if v.HasErrors() {
    return v.Build()
}
```

## 12. Upstream Failures

Calling an external service (payment gateway, OAuth provider, carrier API):

- **Timeout**: wrap as `upstream_timeout`.
- **Non-2xx response**: wrap as `upstream_failure` with the status code in `Internal`.
- **Network error**: wrap as `upstream_failure`.
- **Specific gateway error codes**: translate to semantic codes when meaningful (e.g., Stripe's `card_declined` → `payment_declined`).

**Never leak upstream error messages to clients.** Internal only.

## 13. Database Errors

- **`pgx.ErrNoRows`** → translated by repository to a domain-specific `ErrXxxNotFound`.
- **Unique constraint violations** → translated to `Conflict`.
- **Foreign key violations** → usually a bug; translate to `Internal` and log loudly.
- **Serialization failures** (SERIALIZABLE isolation) → retriable; `yc-dbkit` provides retry helpers.
- **Timeout from `pgx`** → `upstream_timeout`.

Services do not import `pgx` error types outside of the repository layer. The repository translates.

## 14. Event-Handler Errors

Event subscribers (via `yc-eventbus`) use these error signals:

- **Return `nil`** → event processed, ack.
- **Return `eventbus.ErrPoison`** → send straight to DLQ, do not retry.
- **Return any other error** → retry per the configured backoff policy; after exhaustion, DLQ.

```go
func (h *Handler) HandleOrderPlaced(ctx context.Context, evt eventbus.ConsumedEvent) error {
    var p events.OrderPlacedV1
    if err := json.Unmarshal(evt.Payload, &p); err != nil {
        return fmt.Errorf("%w: unmarshal: %v", eventbus.ErrPoison, err)
    }
    if err := h.svc.ProcessOrder(ctx, p); err != nil {
        if errors.Is(err, service.ErrInvalidState) {
            return fmt.Errorf("%w: %v", eventbus.ErrPoison, err)
        }
        return err  // retriable
    }
    return nil
}
```

## 15. Retry

- **Retriable at the client**: 408, 429, 500, 502, 503, 504.
- **Not retriable**: 400, 401, 403, 404, 409, 410, 412, 422.
- Services that call other services use `yc-httpkit`'s retry helper with exponential backoff and max attempts.
- **Idempotency keys** are mandatory for retriable mutations.

## 16. Error Messages for Humans

- Written in **plain English, title case** for the first word.
- No periods at the end of short messages (`"Email is required"`).
- Full sentences for longer messages.
- No stack traces, no internal identifiers, no SQL fragments in user-facing messages.
- Suitable for direct display in a user interface — the UI may still choose to translate or rephrase.

Internal messages (in `Internal`) can be as verbose as needed for operators.

---
