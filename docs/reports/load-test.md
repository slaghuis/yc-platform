# Load Test Report — Phase 9

All three Phase 9 load targets pass on the local stack with rate limits on. That's browsing at 500 requests/s, 10 concurrent checkouts and 1000 events/s through the outbox. Every checkout reconciled across orders, payments and inventory. Getting events to 1000/s took three fixes in yc-eventbus and yc-analytics, described below. Before them, the outbox relay managed 415 events/s and events reached NATS up to 84 s late.

| Scenario | Target | Result |
|---|---|---|
| Browse `/products` | 500 RPS, 3 shops, 60 s | 500 RPS, 0 errors, 0 rate limited; p95 29–35 ms, p99 61–72 ms |
| Checkout | 10 concurrent sessions, 60 s | 3,403 completed (56.6/s), 0 failed; confirm p95 220 ms |
| Events | 1000/s through the outbox, 60 s | relay 1000/s, lag p95 95 ms; both consumers drained 1 s after publishing stopped |

## How to run

```bash
task devstack:up && task devstack:app             # once
task load                                         # all three scenarios
task load -- browse                               # or some of them
RATE=200 DURATION=20s ./scripts/load-test.sh browse checkout
```

`scripts/load-test.sh` provisions three new shops (`load-<run>-1..3`), each with 12 products, 100,000 units of stock per SKU, Stripe and a flat R60 shipping rate. It then runs:

- **browse** — `test/load/k6/browse.js`, a constant arrival rate of `RATE` (500) requests/s. Each virtual user is a visitor of one shop with its own address. The mix is 50% plain list, 20% sorted page of 20, 15% text search and 15% product detail.
- **checkout** — `test/load/k6/checkout.js`, `CHECKOUT_VUS` (10) shoppers checking out back to back. Each iteration is a new visitor: `GET /me`, add to cart, start checkout, set the delivery address, then confirm with the guest's contact and an idempotency key. It pays through the fake Stripe (`services/yc-payment/cmd/fake-stripe`).
- **events** — `test/load/eventload`, which publishes `identity.visitor.created.v1` through yc-eventbus's outbox, exactly as yc-identity does. It publishes `EVENT_RATE` (1000) events/s, spread over the three shops, from a scratch `yc_load` database. It then times the relay and the two durable consumers of that event, yc-analytics and yc-monitor, until they drain.

k6 runs from the `grafana/k6` image on the compose network. The script restarts yc-gateway to trust that network as a proxy (`YC_GATEWAY_TRUSTED_PROXIES`), so each virtual user is rate limited by its own `X-Forwarded-For` address, as it would be behind a load balancer. Rate limits stay at their defaults. The script fails if any k6 threshold or event drain fails. Results (k6 summaries, `events.json`) land in `test/load/results/<run>/`, which is git-ignored.

**Environment.** Apple M5 Pro laptop, Docker Desktop with 15 CPUs and 12 GB of memory. All 17 services, Postgres 16, NATS, SeaweedFS, Jaeger, MailHog and the k6 container shared that machine. Absolute numbers are a floor for production, not a forecast.

## Browse — 500 requests/s

30,001 requests in 60 s, spread evenly across three shops. That's about 167 requests/s each, under the 200/s per-shop limit, so the platform total of 500/s comes from several shops, as decided. There were no failures, no 429s and no 5xx.

| Kind | p50 | p95 | p99 | max | Threshold (p95 / p99) |
|---|---|---|---|---|---|
| list | 4.1 ms | 34.5 ms | 72.0 ms | 154.5 ms | 300 / 800 ms |
| search | 3.9 ms | 32.4 ms | 66.7 ms | 157.0 ms | 400 / 1000 ms |
| detail | 3.4 ms | 29.0 ms | 61.2 ms | 126.8 ms | 300 / 800 ms |

Every request goes through the gateway to yc-product and Postgres; nothing is cached except the gateway's token validation. Search uses yc-product's Postgres full-text index. Neither needed tuning at this rate. A single shop is capped at 200 requests/s (burst 400) by the per-tenant limit, which stays as is.

## Checkout — 10 concurrent sessions

| | p50 | p95 | p99 | max |
|---|---|---|---|---|
| confirm (reserve, pay, order) | 118 ms | 220 ms | 274 ms | 383 ms |
| whole flow (5 requests) | 158 ms | 294 ms | 391 ms | 886 ms |
| any request | 10 ms | 137 ms | 220 ms | 381 ms |

There were 3,403 completed checkouts (56.6/s), 0 that didn't complete and 0 failed requests. Afterwards the databases agreed exactly:

| Check | Count |
|---|---|
| Orders `received` (yc-orders) | 3,403 (1,021 + 1,360 + 1,022 per shop) |
| Payments `captured` (yc-payment) | 3,403 |
| Reservations `committed` (yc-inventory) | 3,403 |
| Stock taken from `on_hand` | 3,403 units, 0 left `reserved` |

There were no lost sagas, no double charges and no leaked reservations.

## Events — 1000 per second

The scenario is 1000 `identity.visitor.created.v1` events a second for 60 s. Each is written to the outbox in its own transaction, relayed to JetStream, and consumed by yc-analytics (a fact row and an aggregate dirty mark) and yc-monitor (a visitor counter).

| Run | Relay rate | Relay lag p50 / max | Outbox drained | Consumers drained | Peak consumer backlog |
|---|---|---|---|---|---|
| Baseline | 415/s | 45.9 s / 84.5 s | 85 s after stop | 87 s after stop | ~2.6k |
| Relay fix | 999/s | 50 ms / 211 ms | 1 s | 66 s | ~34k |
| Consumer fix | 1000/s | 51 ms / 235 ms | 1 s | 1 s | 5.2k |
| Refresher fix | 1000/s | 51 ms / 263 ms | 1 s | 11 s (analytics), 3 s (monitor) | 11.3k |
| Final (full run) | 1000/s | 51 ms / 208 ms | 1 s | 1 s | 4.3k |

The relay lag is the time from the outbox insert to the NATS acknowledgement. Inserts held at 1000/s throughout, with p95 under 7 ms. The last two rows are the same code. The difference between them is Postgres write-ahead-log contention on Docker Desktop (see below).

### What we fixed

1. **The outbox relay published one event at a time and took one batch per poll** (`libs/yc-eventbus/relay.go`). Each poll claimed 100 rows and published them with synchronous JetStream round trips. It then waited for the next tick, so the ceiling was about 100 rows per second per poll interval, whatever the load. Now a poll keeps claiming batches until one comes back short. Each batch is published asynchronously (`PublishMsgAsync`) and the acks are collected with a 10 s timeout. Published rows are marked in one `UPDATE … WHERE id = ANY($1)`. Failure handling is unchanged: retry, and the DLQ after the last attempt. Result: 415 → 1000 events/s, lag 46 s → 50 ms.
2. **Consumers handled one message at a time** (`libs/yc-eventbus/subscribe.go`). A subscription can now set `Concurrency` to handle several messages at once, and the pull buffer grows with it. The default stays 1, because handlers of ordered streams (saga steps, state machines) rely on order. Only handlers that don't depend on order use it. yc-analytics' and yc-monitor's visitor consumers run at 8. Result: the 34k backlog drains with the stream instead of 66 s after it.
3. **The analytics refresher blocked the consumers it feeds** (`services/yc-analytics`). The refresher claimed a batch of dirty days `FOR UPDATE` and rebuilt them in one transaction. Every consumer marking one of those days dirty again waited on the row lock for the whole batch. It also deleted all marks on finishing, so a day marked dirty during its own rebuild could be lost. Now each day is rebuilt in its own short transaction under a per-day advisory lock, and only the marks that existed when the rebuild started are cleared. An integration test holds a mark open during a rebuild and checks that it survives.

### What remains

- **Write-ahead-log throughput on Docker Desktop.** In some runs yc-analytics slows to about 600/s for 20–30 s and catches up within seconds after the load stops. Postgres waits are then dominated by `LWLock:WALWrite` and `IO:WALSync`. Each event costs the consumer two commits: the handler's transaction, then the `processed_events` dedup row. Accepted for v1, because the consumer still drains and no event is lost. v2 backlog: write the dedup row in the handler's transaction. That halves commits and makes handling exactly-once per consumer.
- **Hot aggregate rows.** Every visitor of a shop on a day marks the same `agg_dirty` row, so concurrent handlers queue on it (`Lock:transactionid`). This is cheap at today's volume. If it shows up in production, batch the marks in memory or move to an append-only marks table.

## Connection pools

Every service opens a pgx pool of up to 10 connections (`database.max_conns`). With 17 services that's up to 170, plus migrations and admin sessions. Postgres's default `max_connections` of 100 was hit under load, so the dev stack now starts Postgres with `max_connections=300` (`docker-compose.yml`).

In production, the sum of every replica's pool must fit under `max_connections`, with headroom for migrations and operators: \(\sum_{\text{services}} \text{replicas} \times \text{max\_conns} < \text{max\_connections} - 20\). Once replicas are scaled out, put PgBouncer in transaction mode in front of Postgres instead of raising `max_connections`. Tenant context is set per transaction (`set_config(..., true)`) and the advisory locks are transaction-scoped, so transaction pooling is safe. Prepared statements need PgBouncer 1.21+ with `max_prepared_statements` set, or pgx's `default_query_exec_mode=exec` in the database URL. Migrations take a session-level advisory lock (golang-migrate), so `migrate_on_start` and `yc-migrate` must connect to Postgres directly, not through PgBouncer. Raise `max_conns` only for services that show pool waits (`pgxpool` `EmptyAcquireCount`). In this test no service needed more than 10.

## Findings for other Phase 9 work

- **A restarted gateway served 404/503 for up to 30 s.** Routes live in yc-gateway's memory, and services re-registered every 30 s. During that window, routes registered with the old gateway didn't exist. This was seen when the load script restarted the gateway. Deployment prep cut the interval to 5 s and keeps a restarted gateway unready until then (see `docs/reports/chaos-test.md`).
- **Rate limits behind a load balancer.** Without `trusted_proxies`, every anonymous visitor shares the balancer's address and its 120/min limit, which works out to about 2 requests/s for a whole shop. Setting `trusted_proxies` to the balancer's range is a deployment requirement (see the security review).
