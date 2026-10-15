# API Conventions

Conventions for HTTP APIs exposed by services and consumed through yc-gateway. These apply equally to public, internal, and admin endpoints.

## 1. Protocol

- **REST over HTTP/1.1 and HTTP/2.** No gRPC in v1.
- **JSON** request and response bodies. `Content-Type: application/json; charset=utf-8`.
- **TLS everywhere in production.** HTTP in dev through compose.

## 2. OpenAPI as Source of Truth

- Every service maintains `api/openapi.yaml` (OpenAPI 3.1).
- Handlers and client code are generated from this spec via `oapi-codegen`.
- The spec is reviewed as part of PRs that change endpoints.
- Schema types in the spec correspond directly to Go structs.

## 3. URL Structure

### Prefixes by audience

| Prefix | Audience | Auth |
|---|---|---|
| `/` (root) | Public — visitors and customers | Any token |
| `/admin/` | Shop admins | Role: shop_admin or system_admin |
| `/sysadmin/` | System admins | Role: system_admin |
| `/internal/` | Service-to-service | Service token |
| `/webhooks/` | External providers | Signature verification |

Audience is encoded in the URL so middleware can enforce auth without parsing further.

### Resource naming

- **Plural nouns for collections**: `/products`, `/orders`.
- **IDs in path for individuals**: `/products/{id}`.
- **Sub-resources are nested**: `/products/{id}/images`.
- **Actions on resources use verbs under the resource**: `POST /orders/{id}/cancel`.
- **No verbs in top-level URLs.** `/search-products` is wrong; `/products` with query parameters is right. The exception is actions clearly not part of a resource (e.g., `/login`, `/logout`).

### Versioning

- **Not in the URL path.** The event schemas carry versions; APIs do not.
- **Breaking changes require a new endpoint or query parameter**, not a `/v2/` prefix. In practice, breaking changes should be rare; prefer additive evolution.
- **If versioning becomes necessary**, use an `Accept` header: `Accept: application/vnd.yc.v2+json`.

## 4. HTTP Methods

| Method | Use | Idempotent | Safe |
|---|---|---|---|
| GET | Read | Yes | Yes |
| POST | Create or action | No | No |
| PUT | Replace | Yes | No |
| PATCH | Partial update | No (unless explicit) | No |
| DELETE | Remove | Yes | No |

- **Use PUT for full-document replacement**, PATCH for partial updates.
- **POST for actions that don't fit CRUD**: `POST /orders/{id}/cancel`, `POST /checkout/{id}/confirm`.

## 5. Status Codes

Only these status codes should appear in responses:

| Code | Meaning | Use |
|---|---|---|
| 200 | OK | Successful GET, PUT, PATCH, DELETE, action POST |
| 201 | Created | Successful POST that creates a resource |
| 202 | Accepted | Async processing started (rare) |
| 204 | No Content | DELETE with no body, action with no response |
| 400 | Bad Request | Validation error, malformed JSON |
| 401 | Unauthorized | Missing or invalid token |
| 403 | Forbidden | Authenticated but not permitted |
| 404 | Not Found | Resource doesn't exist (within authorized scope) |
| 409 | Conflict | Resource state conflict (e.g., duplicate email) |
| 410 | Gone | Resource deleted and will not return |
| 412 | Precondition Failed | If-Match or similar failed |
| 422 | Unprocessable Entity | Semantic validation (vs 400 syntactic) — use sparingly |
| 429 | Too Many Requests | Rate limited |
| 500 | Internal Server Error | Unexpected server failure |
| 502 | Bad Gateway | Upstream service failed |
| 503 | Service Unavailable | Deliberately unavailable (maintenance, tenant suspended) |
| 504 | Gateway Timeout | Upstream service timed out |

**Not permitted**: 418, custom status codes, any 3xx (we are an API, not a browser-facing app).

## 6. Response Envelope

### Success

Return the resource directly:

```json
{
  "id": "abc-123",
  "name": "Widget",
  "created_at": "2025-01-15T10:00:00Z"
}
```

For collections, wrap in a container that supports pagination:

```json
{
  "items": [...],
  "page": {
    "size": 20,
    "cursor": "...",
    "next_cursor": "...",
    "total_available": 142
  }
}
```

Do NOT wrap single resources in `{ "data": ... }`. Verbose and adds no value.

### Error

Problem+JSON (RFC 7807) adapted:

```json
{
  "type": "yc:validation_failed",
  "title": "Validation failed",
  "status": 400,
  "detail": "The email field is required.",
  "code": "validation_failed",
  "correlation_id": "ec1a...",
  "errors": [
    {
      "field": "email",
      "code": "required",
      "message": "Email is required."
    }
  ]
}
```

Fields:
- `type` (required) — stable identifier, `yc:<code>` form
- `title` (required) — human-readable summary, English
- `status` (required) — HTTP status
- `detail` (optional) — specific explanation for this occurrence
- `code` (required) — stable machine-readable code
- `correlation_id` (required) — request's correlation ID for support cross-reference
- `errors` (optional) — field-level details for validation errors

Produced via `yc-errors.WriteError(w, err)`.

## 7. Pagination

- **Cursor-based by default.** No offset/limit pagination.
- **Opaque cursors.** Clients treat them as strings; services encode what they need (last ID, timestamp).
- **Standard query parameters**: `page_size` (max 100, default 20), `cursor`.
- **Response includes `next_cursor`** when more results exist; absent means last page.
- **`total_available` is a best-effort estimate** when cheap to compute; omit when expensive.

## 8. Filtering and Sorting

### Filtering

- Query parameters named after fields: `?status=active&category=electronics`.
- Multiple values: comma-separated: `?status=active,pending`.
- Ranges: `?price_min=100&price_max=500`.
- No DSL in query strings. Complex queries belong in POST bodies with a documented filter schema.

### Sorting

- `sort` parameter with comma-separated field names.
- Prefix with `-` for descending: `?sort=-created_at,name`.
- Services define which fields are sortable in their OpenAPI spec.

## 9. Request Headers

Standard headers that every endpoint handles:

| Header | Set By | Purpose |
|---|---|---|
| `Authorization: Bearer <token>` | Client | Access token |
| `X-YC-Tenant-ID` | yc-gateway | Resolved tenant |
| `X-YC-Correlation-ID` | yc-gateway or client | Trace correlation |
| `X-YC-Actor-ID` | yc-gateway | Authenticated actor |
| `X-YC-Actor-Type` | yc-gateway | visitor, customer, shop_admin, system_admin |
| `Accept-Language` | Client | Preferred locale (BCP 47) |
| `X-YC-Display-Currency` | Client | Override display currency (optional) |
| `Idempotency-Key` | Client | For retriable mutations (optional) |

- **Services never trust client-supplied tenant or actor headers directly.** Only trust the ones set by yc-gateway after token validation.
- **Internal endpoints** check these headers too; service-to-service calls through yc-gateway propagate them.

## 10. Idempotency

- **GET, PUT, DELETE are naturally idempotent.**
- **POST that creates or acts should accept `Idempotency-Key`.**
- **Idempotency keys are UUIDs or client-generated opaque strings** scoped to the endpoint.
- **The service stores processed keys** for 24h minimum. Replays return the previously-computed response.
- Required on: `POST /checkout`, `POST /checkout/{id}/confirm`, `POST /admin/orders/{id}/refund`, and any other operation with financial or inventory consequences.

**Implementation.** `httpkit.Idempotency` is the standard middleware. Place it after authentication; it stores the response to each POST that carries a key in the service's `idempotency_keys` table (copy `libs/yc-httpkit/idempotency.sql` into a migration, as yc-inventory and yc-orders do) and purges entries after `idempotency.retention` (24 h).

- Keys are scoped to the tenant, the caller (service or actor), the method and the path.
- A repeated key replays the stored status and body, with `Idempotent-Replayed: true`.
- The same key with a different body is `422`; while the first request is still running it is `409`.
- 5xx responses are not stored, so a retry runs again.
- `Required: true` rejects a POST without a key (`400`).

A service may instead be idempotent natively when replaying a stored response would be wrong, because a retry must learn the current state rather than the first answer: yc-payment's authorize and refund look the transaction up by key, and yc-checkout stores the confirm key on the session (`docs/services/yc-checkout.md`). `POST /checkout` itself does not take a key: a repeated start only leaves an extra quote that expires unused.

## 11. Content Types

- **Request body**: `application/json`.
- **Response body**: `application/json`.
- **File uploads**: `multipart/form-data`.
- **Webhooks**: `application/json`; signature verification per gateway.
- **Health endpoints**: `text/plain` is acceptable for `/healthz`; JSON for `/readyz` with detail.

## 12. Date and Time

- **All timestamps are RFC 3339 UTC**: `2025-01-15T10:00:00Z`.
- **Dates without time**: `2025-01-15` (ISO 8601).
- **Durations in explicit units**: `expires_in_seconds`, not `expires_in`.
- **No epoch timestamps** in payloads; they're unreadable.

## 13. Money

- **All monetary amounts are integers in minor units** (cents, kobo, etc.).
- **Always paired with a `currency` field** (ISO 4217).
- **Field names**: `amount_minor`, `price_minor`, `total_minor`. The `_minor` suffix is a reminder.
- **Never use floats.** `yc-money.Money` is the canonical type in Go.

```json
{
  "amount_minor": 12995,
  "currency": "ZAR"
}
```

## 14. Enumerations

- **Lowercase, snake_case strings.** Not uppercase, not numeric.
- **Defined in OpenAPI spec as `enum`**; services generate Go constants.
- **Add values freely; never remove or rename.** If a value is wrong, add a new one and deprecate the old.

```yaml
status:
  type: string
  enum: [pending, approved, rejected]
```

## 15. Field Naming

- **snake_case** in all JSON. Not camelCase, not PascalCase.
- **No Hungarian notation.** `is_active` is fine; `b_active` is not.
- **Booleans named as predicates**: `is_active`, `requires_shipping`, `has_tracked_items`.
- **IDs**: suffix with `_id`: `user_id`, `product_id`.
- **Timestamps**: suffix with `_at`: `created_at`, `expires_at`.
- **Durations**: suffix with units: `ttl_minutes`, `expires_in_seconds`.
- **Counts**: suffix with `_count`: `item_count`, `rating_count`.

## 16. Null vs Absent

- **Required fields are always present** with a meaningful value.
- **Optional fields that are set to null** should be explicitly `null`, not omitted.
- **Partial updates (PATCH)**: omitted fields mean "do not change"; explicit `null` means "clear".
- **Document this behavior** in OpenAPI with `nullable: true` where applicable.

## 17. Health Endpoints

Every service exposes:

- `GET /healthz` — liveness. Returns 200 if the process is up. No dependencies checked.
- `GET /readyz` — readiness. Returns 200 only when the service can serve traffic (DB connected, NATS connected, migrations applied). Returns 503 otherwise with detail:

```json
{
  "status": "not_ready",
  "checks": {
    "database": "ok",
    "nats": "connecting",
    "migrations": "ok"
  }
}
```

- `GET /version` — build info. Returns version, commit hash, build time.

These endpoints are **always unauthenticated** and must be fast.

## 18. CORS

- yc-gateway handles CORS centrally.
- Services do not set CORS headers.
- Allowed origins configured per tenant.

## 19. Rate Limiting

- yc-gateway enforces rate limits.
- Services receive already-filtered traffic.
- A 429 from a service means the service itself hit a backend limit (e.g., gateway API quota), not that the caller is being rate-limited by us.
- Services expose `/metrics`-style counters internally so yc-monitor can observe load.

## 20. Deprecation

When an endpoint is to be removed:
1. Document deprecation in OpenAPI with `deprecated: true`.
2. Add response header `Deprecation: true` and `Sunset: <date>`.
3. Announce in release notes.
4. Minimum deprecation period: 90 days before removal.

---
