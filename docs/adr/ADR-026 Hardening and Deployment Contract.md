# ADR-026: Hardening and Deployment Contract

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Phase 9 hardens the platform before the first tenant: a security review, load tests (500 requests/s browsing, 10 concurrent checkouts, 1000 events/s) and chaos drills (`docs/reports/`). They settled questions earlier ADRs left open: where the platform will run, what an image promises to whatever runs it, how a service stops without dropping work, how many replicas are safe, how rate limits apply behind a load balancer, and whether analytics needs its own store. The first tenant's hosting is not chosen yet, so the decision is a contract any target must meet rather than a target.

## Decision

**The platform stays target-neutral: each service is one image that meets the contract below, and the hosting target is chosen with the first tenant. Until registrations and limits are shared, every service, including the gateway, runs as one replica. Analytics stays on Postgres. Rate limits stay per shop, with the platform's throughput coming from many shops.**

## Rules

### Deployment contract

1. **Target-neutral.** No orchestrator, cloud or manifest format is assumed. Compose (`docker-compose.yml`) is the reference deployment; a production target is chosen with the first tenant and must satisfy rules 2–9. The choice gets its own ADR.
2. **Images.** One image per service from `deploy/docker/Dockerfile.service`: a static binary on `distroless/static` as `nonroot`, listening on `:8080` (yc-gateway also on `:8091`, its internal listener). Built with `--build-arg SERVICE=… VERSION=…`; `VERSION` (the release tag) is stamped into the binary and the image label. Configuration is the embedded `config.default.yaml` overridden by environment variables (`YC_<SERVICE>__<PATH>`); secrets come from the environment or mounted files (`YC_IDENTITY_JWT_PRIVATE_KEY_FILE`, `YC_CONFIG_FILE`), never from the image.
3. **Probes.** `GET /healthz` is liveness: the process serves HTTP; it checks no dependency. `GET /readyz` is readiness: Postgres and NATS answer, and the service is not draining. Liveness must never use `/readyz`, or a database outage restarts every service at once. For runtimes without HTTP probes the binary checks itself: `/app/service healthcheck` exits 0 when `/healthz` answers 200; the image's `HEALTHCHECK` uses it.
4. **Graceful shutdown.** On `SIGTERM` a service answers `/readyz` with `503 draining` for `http.shutdown_delay`, then stops accepting connections, gives in-flight requests up to `http.shutdown_timeout` (20 s), then stops subscribers and the outbox relay and closes the database. `shutdown_delay` is 0 by default; behind a load balancer set it longer than the balancer takes to notice an unready target (5 s with typical probe settings). Closing takes up to 10 s more and flushing telemetry 5 s, so the runtime's grace period must exceed `shutdown_delay + shutdown_timeout + 15 s`; compose uses 40 s. A service killed outright loses nothing committed: sagas resume from the database (ADR-019), events from the outbox (ADR-006).
5. **One replica per service.** yc-gateway keys registrations by service name (ADR-020), so replicas of a service would replace each other's routes, and one replica stopping would deregister them all. The gateway's rate-limit buckets and token cache are per instance. Scaling out needs per-instance registrations and a shared limiter (v2 backlog). yc-checkout's lease-based sweeper (ADR-019) already tolerates replicas.
6. **Registration.** Services re-register every 5 s (`gateway.interval`); an unchanged registration only resets the health-failure count. A restarted gateway reports unready for `registry.startup_grace` (10 s) so a load balancer does not send it traffic before routes are back.
7. **Network.** Only yc-gateway's public listener is exposed, behind TLS. The gateway's internal listener and every service's port stay on the private network: route registration there is unauthenticated (security review finding 7). `YC_GATEWAY__TRUSTED_PROXIES` is set to the load balancers' addresses so visitors are limited by their own address, and the visitor cookie stays `Secure`.
8. **Database connections.** Every replica's pool (`database.max_conns`, default 10) together stays under Postgres's `max_connections` minus 20 for migrations and operators. With more replicas, PgBouncer in transaction mode goes in front; migrations connect directly (they take a session-level advisory lock).
9. **Releases.** Before a release that touches the saga, the event bus or the gateway, `task load` and `scripts/chaos-test.sh` run against a stack built from the release and must pass.

### Throughput and limits

10. **Rate limits** stay as configured in yc-gateway: 200 requests/s per shop (burst 400), 600/min per signed-in actor (burst 60) and 120/min per visitor address (burst 30), with per-shop overrides. The 500 requests/s target is the platform across shops, not one shop.
11. **Login throttle.** yc-identity refuses sign-ins for a shop and email after 10 failures in 15 minutes (`429`), counting unknown emails alike. A successful login or a completed password reset clears the count.
12. **Event throughput.** The outbox relay drains in batches until a batch comes back short, publishing each batch asynchronously. A consumer handles one message at a time unless its subscription sets `Concurrency`, which only handlers that don't depend on order may use (today yc-analytics' and yc-monitor's visitor consumers, at 8).
13. **Analytics stays on Postgres** (ADR-025 rules 13–14). At 1000 events/s it keeps up, with each shop-day's aggregates rebuilt in its own transaction. Revisit when facts outgrow a single Postgres or rebuilds fall behind; the load report records the first bottlenecks (write-ahead-log throughput, hot dirty-mark rows).

## Consequences

Positive:

- Any container runtime can host the platform; choosing one later costs a manifest, not code.
- Rolling restarts drop no requests behind a load balancer, and a killed service loses no work: the chaos drills killed and paused every saga service, the gateway, identity, NATS and Postgres mid-checkout, and the books reconciled.
- One replica per service keeps the registry and limits simple and correct.

Negative:

- No horizontal scaling and no high availability for a single service in v1: a service restart is a short outage of its routes (seconds for most; the gateway's startup grace is 10 s). Load testing put the single-replica ceiling well above v1's expected traffic.
- Re-registering every 5 s is 17 small requests every 5 s to the gateway's internal listener.
- `shutdown_delay` must be tuned per environment; left at 0 behind a load balancer, a few requests can land on a stopping instance and fail.

## Alternatives Considered

- **Choose Kubernetes (or a managed container service) now**: rejected; nothing in the first tenant's requirements forces it, and the contract keeps the choice open at no cost.
- **Several replicas per service now**: rejected; the gateway registry, limiter and token cache would need shared state first (v2).
- **Persist gateway registrations** so a restarted gateway serves at once: deferred; the 10 s startup grace with 5 s re-registration achieves the same behind a load balancer without new state.
- **A separate analytics store (ClickHouse, TimescaleDB)**: rejected for v1; Postgres met the target.
- **Raising the per-shop rate limit to 500/s**: rejected; no single shop needs it, and the per-shop limit protects shops from each other.
