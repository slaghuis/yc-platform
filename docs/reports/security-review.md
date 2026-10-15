# Security Review — Phase 9

**Result: no open critical or high findings.** The review fixed one high and four medium or low findings. Three items remain as accepted risks or deployment requirements. The automated scans are clean: govulncheck has nothing reachable, the 14 gosec suppressions are reviewed, and gitleaks finds nothing in the git history. Sign-off is the platform owner's (see the end of this report).

Scope: all 17 services, the shared libraries and tools at the `phase-9` branch, on the public listener (yc-gateway) and the service-to-service paths behind it. Out of scope: the deployment target (to be chosen with the first tenant, ADR-026) and the storefront and admin front ends, which don't exist yet.

## Findings

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High | Invalid credentials skipped the gateway's rate limits | Fixed |
| 2 | Medium | No limit on password guessing per account | Fixed |
| 3 | Medium | Behind a load balancer every anonymous visitor shares one rate-limit bucket | Fixed (needs `trusted_proxies` in deployment) |
| 4 | Low | No browser security headers | Fixed |
| 5 | Low | yc-audit accepted request bodies of any size | Fixed |
| 6 | Low | The login throttle lets anyone lock an email out | Mitigated |
| 7 | Medium | Route registration on the internal listener is unauthenticated | Accepted: deployment requirement, v2 backlog |
| 8 | Low | Rate limits and the token cache are per gateway instance | Accepted: v2 backlog |
| 9 | Info | GO-2026-5932 (`golang.org/x/crypto/openpgp`) in a required module | Accepted: not imported |
| 10 | Medium | Outdated dependencies in the CI pipeline module (gRPC, `x/net`, `x/text`) | Fixed |

### 1. Invalid credentials skipped the rate limits — High, fixed

yc-gateway limited requests without credentials by client address, and everything else by the actor behind a valid token. A request with an invalid bearer token or a stale visitor cookie fell between the two. For each such request, the gateway asked yc-identity to validate the token (rejections aren't cached), or minted a fresh visitor for the stale cookie, with no limit at all. Anyone could flood yc-identity or farm visitors without limit.

**Fix:** invalid tokens and replaced cookies now count against the client address, exactly like no credentials (`gateway.go`, `identify`). Covered by `TestRateLimit` and the e2e test "invalid credentials do not dodge the rate limits".

### 2. Unlimited password guessing per account — Medium, fixed (OWASP A07)

The per-address limit (120/min) slows one attacker, but a botnet spreads credential stuffing across many addresses, and nothing limited guesses against one account.

**Fix:** yc-identity counts failed sign-ins per shop and email (`login_failures`, under RLS). After 10 failures in 15 minutes, logins for that email get 429, even with the right password. Unknown emails are counted the same way, so the response doesn't reveal which emails are registered. A successful login clears the count. Configured with `password.login_max_failures` and `password.login_failure_window`.

### 3. One rate-limit bucket behind a load balancer — Medium, fixed

The gateway took the client address from the TCP connection and ignored `X-Forwarded-For`, correctly, since clients can forge it. Behind a load balancer, though, every anonymous visitor shared the balancer's address and its visitor quota of 120/min. That's about 2 requests a second for a whole shop, so a single scraper could take a storefront offline.

**Fix:** `trusted_proxies` (CIDRs or IPs). A request from a trusted proxy is attributed to the rightmost `X-Forwarded-For` entry that isn't itself trusted, and entries to its left, which the client controls, are ignored. With nothing configured, the header is still ignored. **Deployment requirement:** set `YC_GATEWAY__TRUSTED_PROXIES` to the load balancers' range. The load test runs this way (`docs/reports/load-test.md`).

### 4. No browser security headers — Low, fixed (OWASP A05)

Every public response now carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, `Referrer-Policy: no-referrer` and `Strict-Transport-Security: max-age=31536000`. The API serves only JSON and redirects, so the strictest policy costs nothing.

### 5. yc-audit had no body size limit — Low, fixed

Every other service caps request bodies with `httpkit.LimitBody`. yc-audit's router now does too, at the default 1 MiB.

### 6. Login lockout as denial of service — Low, mitigated

Fix 2 lets anyone who knows an email refuse its sign-ins for 15 minutes, renewable for as long as they keep failing. This is the usual trade-off of account throttling. Mitigation: a completed password reset clears the count, so the owner of the email can always get back in. The window is short and the limit configurable (0 turns it off). Covered by the "password reset" subtest in yc-identity's integration tests.

### 7. Unauthenticated route registration — Medium, accepted with a deployment requirement

Services register their routes with `POST /internal/register` on the gateway's internal listener (`:8091`) without credentials (ADR-020 rule 6; ADR-021 deferred it). Anyone who can reach that listener can register a route and receive traffic for it. Service-to-service calls on the same listener do require service tokens.

**Requirement:** the internal listener must not be reachable from outside the platform network (separate port, not published, network policy or security group). **v2 backlog:** require a service token for registration.

### 8. Per-instance limits and token cache — Low, accepted

Rate-limit buckets and the 60 s token-validation cache live in each gateway instance's memory. With N instances the effective limits are N times the configured ones, and a revoked token can work for up to 60 s on an instance that cached it. This is fine for a single instance. **v2 backlog:** Redis-backed limiter and cache before running several gateways (`docs/services/yc-gateway.md`, future scope).

### 9. GO-2026-5932 — Info, accepted

`golang.org/x/crypto/openpgp` is unmaintained and unsafe by design, and there is no fixed version. It is in the module graph of 23 modules through `golang.org/x/crypto` v0.58.0, but no code imports the package: govulncheck reports it only under "modules you require" (`-show verbose`), never as reachable.

### 10. Outdated dependencies in the CI pipeline module — Medium, fixed

Trivy found 16 HIGH and CRITICAL advisories in `pipeline/go.mod`, the Dagger-based CI wrapper. They included a critical gRPC-Go authorization bypass (CVE-2026-33186) and denial-of-service fixes in `golang.org/x/net` and `x/text`. The module isn't in `go.work` or any image, and it uses gRPC only as a client of the Dagger engine, so production was never exposed. A compromised CI tool is still a supply-chain risk, though. **Fix:** gRPC 1.84.0, `x/net` 0.61.0, `x/text` 0.43.0, `x/mod` 0.42.0 and OpenTelemetry 1.44.0, all indirect. `pipeline-lib` v0.2.0 is already the latest; the module builds. govulncheck had only covered the workspace modules, which is why this was missed. It now reports nothing reachable in the module. Three advisories remain in OpenTelemetry's log and trace exporters (GO-2026-6615, -6508, -6505), but only Dagger v0.14's telemetry uses them. Fixing those needs a `pipeline-lib` release on a newer Dagger, which is on the v2 backlog.

## Automated scans

| Tool | Scope | Result |
|---|---|---|
| govulncheck (latest) | all 33 workspace modules | 0 reachable vulnerabilities; GO-2026-5932 in required modules only (finding 9) |
| gosec (in golangci-lint, every module, with integration tests) | all non-test code | 0 issues; 14 inline suppressions reviewed (below) |
| gitleaks v8.30.1 | full git history (35 commits) | no leaks |
| gitleaks v8.30.1 | working tree | 7 hits, none real: `.env` and the generated dev JWT key are git-ignored; the rest are test fixtures allowlisted in `.gitleaksignore` |
| Trivy 0.69.1 `fs` (vulnerabilities, misconfigurations, secrets) | repository: 35 targets, every `go.mod`, the Dockerfile and compose | no HIGH or CRITICAL after finding 10 was fixed |
| Trivy 0.69.1 `image` | `yc-platform/yc-orders:dev` (distroless Debian 12, 40 MB; every service image has the same base) | no HIGH or CRITICAL |

gitleaks also runs as a pre-commit hook (`.pre-commit-config.yaml`). `G101` (hard-coded credentials) is excluded globally because config field names trip it; gitleaks covers that ground.

**gosec suppressions reviewed** — all are integer conversions bounded by the protocol, file paths from operator configuration or the repository itself, or dev-only tools:

- NATS delivery counts and stream sequences narrowed to `int`/`int64`: `libs/yc-eventbus` (3), `services/yc-monitor` (2).
- `uint32(len(key))` for argon2: `services/yc-identity/internal/password`.
- Reading the JWT key from its configured path: `services/yc-identity/internal/token`.
- Marshalling credentials that are sealed before storage: `services/yc-payment/internal/service`.
- The visitor cookie's `Secure` flag, configurable for plain-HTTP dev and true by default: `services/yc-gateway`.
- Schema generator file I/O: `tools/yc-schema-gen` (3).
- The fake Stripe's outbound webhook: `services/yc-payment/cmd/fake-stripe`.
- Load-test counter clamp: `test/load/eventload`.

## OWASP Top 10 (2021)

| Risk | Where the platform stands |
|---|---|
| A01 Broken access control | Every shop's data sits behind forced row-level security keyed on `app.tenant_id` (ADR-017). `testkit.AssertTenantIsolation` runs in all 16 services with tenant tables and fails if a tenant table lacks forced RLS or the service role can bypass it. Only the outbox (drained across tenants by the relay) and yc-gateway's `tenant_hosts` (read to find the tenant) are exempt. The gateway binds tokens to their shop's hosts and strips client-sent `X-YC-*` headers. Each service checks roles. The e2e `TestIsolation` covers foreign credentials, foreign IDs, role routes, internal routes, forged identity headers and odd paths. |
| A02 Cryptographic failures | Passwords use argon2id (64 MiB, t=2). Access tokens are RS256 with the algorithm pinned on verification. Refresh, reset and visitor tokens are stored as SHA-256 hashes. Payment gateway credentials are sealed with AES-256-GCM, with associated data binding each blob to its row, so a blob copied to another shop won't open. HSTS is set, and the visitor cookie is `Secure` by default. |
| A03 Injection | All SQL uses pgx placeholders. The 29 `fmt.Sprintf` query builders interpolate only placeholder numbers, integer limits and constant table or column names, and search terms go through `websearch_to_tsquery` as parameters. Event payloads are schema-validated on publish. There are no shell or template sinks. |
| A04 Insecure design | Tenant, actor and visitor rate limits, the login throttle, idempotency keys on payments and checkout, outbox and saga compensation (ADR-006/018), and DLQ replay only after human review. |
| A05 Security misconfiguration | Security headers (finding 4), body limits on every service, `log_reset_links` and fake providers refused in production, and no CORS (same-origin only). Production config is reviewed in deployment prep. |
| A06 Vulnerable components | govulncheck and Trivy run in the pipeline and block on HIGH. Earlier phases bumped `golang.org/x/crypto` past CVE-2026-56854; it is at v0.58.0. |
| A07 Identification and authentication | Login throttle (finding 2), uniform 401 and timing for unknown emails (`VerifyNothing`), refresh-token rotation with reuse detection, sessions revoked on password reset, single-use reset tokens, 10-character minimum password. |
| A08 Software and data integrity | Stripe and Yoco webhooks are signature-verified (Yoco with Standard Webhooks). Every event is validated against its JSON Schema on publish and consume. Migrations are embedded in the binaries. |
| A09 Logging and monitoring | yc-audit keeps an append-only trail of security-relevant events (its service user can't update or delete records, and a trigger refuses it for everyone). yc-monitor alerts on DLQ rate and service health. Structured logs carry correlation IDs. Secrets are masked (payment key hints; reset tokens only with the dev-only flag). |
| A10 Server-side request forgery | No endpoint fetches a client-supplied URL. Outbound calls go to configured hosts only: FX rates, OAuth providers, Stripe and Yoco, the critical-alert webhook, and registered services on the internal network (finding 7). Images are uploaded straight to object storage with presigned URLs. |

## Deployment requirements from this review

1. Set `YC_GATEWAY__TRUSTED_PROXIES` to the load balancers' addresses (finding 3).
2. Keep yc-gateway's internal listener and every service's port off the public network (finding 7).
3. Leave `YC_GATEWAY_COOKIE_SECURE` at its default `true` and terminate TLS in front of the gateway.
4. Run one gateway instance until the limiter and token cache are shared (finding 8).

ADR-026 makes them part of the deployment contract (rules 5 and 7).

## Sign-off

| | |
|---|---|
| Reviewed by | Phase 9 hardening (automated scans and code review) |
| Open critical | 0 |
| Open high | 0 |
| Accepted | findings 7, 8, 9 |
| Signed off by | _pending — platform owner_ |
