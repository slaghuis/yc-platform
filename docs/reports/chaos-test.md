# Chaos Test Report — Phase 9

All 13 drills pass on the local stack. Each drill killed or froze one part of the platform during live checkouts. Every saga settled within a second of traffic stopping, and orders, payments and stock agreed exactly afterwards: no lost sagas, double charges or leaked reservations. Shoppers did see errors while a service was down. Those errors are listed below, and each matches the saga's design: a request fails fast, or the saga compensates and the shopper can retry.

| Drill | What broke | Checkouts completed | Shopper errors | Sagas failed (compensated) | Back | Settled |
|---|---|---|---|---|---|---|
| kill-checkout | yc-checkout killed 20 s | 970 | 80 | 0 | 1 s | 1 s |
| kill-cart | yc-cart killed 20 s | 1,303 | 63 | 0 | 0 s | 1 s |
| kill-pricing | yc-pricing killed 20 s | 1,209 | 84 | 0 | 0 s | 1 s |
| kill-inventory | yc-inventory killed 20 s | 1,408 | 36 | 35 | 1 s | 1 s |
| kill-payment | yc-payment killed 20 s | 958 | 52 | 1 | 1 s | 1 s |
| kill-orders | yc-orders killed 20 s | 917 | 36 | 36 | 1 s | 0 s |
| kill-shipping | yc-shipping killed 20 s | 590 | 48 | 0 | 0 s | 0 s |
| kill-gateway | yc-gateway killed 20 s | 927 | 8 | 0 | 10 s | 0 s |
| kill-identity | yc-identity killed 20 s | 1,315 | 84 | 0 | 0 s | 1 s |
| nats | NATS killed 20 s | 1,103 | 0 | 0 | 5 s | 0 s |
| pause-payment | yc-payment frozen 45 s | 1,476 | 44 | 0 | 0 s | 0 s |
| pause-inventory | yc-inventory frozen 45 s | 1,544 | 44 | 40 | 0 s | 0 s |
| pause-postgres | Postgres frozen 45 s | 1,531 | 6 | 2 | 0 s | 0 s |

"Back" is how long the restarted or unfrozen container took to report ready. "Settled" is how long after traffic stopped every checkout session left the saga states. Checkouts completed counts sessions in `COMPLETED`, which can be a few more than the shoppers saw complete (see below).

## How to run

```bash
task devstack:up && task devstack:app             # once
./scripts/chaos-test.sh                           # every drill, ~45 min
./scripts/chaos-test.sh kill-orders nats          # or some of them
```

Each drill gets its own shop with 3 products, 100,000 units of stock each, Stripe (the fake, `services/yc-payment/cmd/fake-stripe`) and flat R60 shipping. Four k6 shoppers (`test/load/k6/checkout.js`) check out back to back. After 10 s of normal traffic the drill strikes: `docker kill` for 20 s then `docker start`, or `docker pause` for 45 s then `docker unpause`. Traffic continues for 30 s after recovery. Then the script waits up to 300 s for every saga to settle and checks the shop's books:

- no checkout session is left mid-saga;
- each completed checkout has exactly one live order, `received`;
- each completed checkout's payment is captured, and no other payment is captured or left open;
- each completed checkout's reservation is committed and no other is held, and the stock taken equals the units committed, with nothing left reserved.

A 45 s pause outlasts every caller's timeout (35 s for payment calls, 10 s for internal calls), so the pause drills cover "slow dependency" as well as "frozen dependency". The NATS drill also checks that every service's outbox drains and every durable consumer catches up. Results land in `test/load/results/<run>/` (`chaos.json`, one k6 summary and log per drill). The environment is the one in the load test report: an Apple M5 Pro laptop with Docker Desktop.

## What shoppers saw

| Error | Drills | Why |
|---|---|---|
| `502` on the dead service's step | checkout, cart, pricing, payment, shipping, identity | The gateway can't reach the service. Nothing was written; the shopper retries. |
| `404` on the dead service's step | checkout (28), cart (30) | The gateway deregistered the service after failed health checks, so its public routes no longer exist (see the findings). |
| `202` on confirm | inventory, payment, orders, pause-payment, pause-inventory | The saga couldn't finish within the request and carries on in the background. It either completes or compensates to `FAILED`; the shopper polls the session. |
| `422` on confirm | pause-postgres (2) | The saga compensated before the request returned. |
| `504` | pause-postgres (3) | `GET /me` waited on the frozen database past the gateway's timeout. |
| `HTTP 0` (connection refused) | kill-gateway (8) | Nothing listens on the gateway's port until it restarts. |

When yc-inventory or yc-orders is down, in-flight confirms compensate. The payment is voided or refunded and the reservation released, so those sessions end `FAILED`. Starting a checkout asks yc-payment which gateway to use, so while yc-payment is down new checkouts fail at the start (`502`) and few sagas need compensating.

**Confirms that "failed" but completed.** In kill-checkout, 4 confirms returned `502` because yc-checkout died after the saga had started; the sweeper resumed those sagas once it restarted. In kill-gateway, 4 confirms lost their connection while yc-checkout carried on. In all 8 cases the order and captured payment exist. A shopper who saw the error and gave up has still bought the goods, which is the right outcome. Clients should still poll the session after a failed confirm instead of reporting failure (v2 backlog).

**Sessions left in `PRICING`.** In kill-shipping, 48 sessions stayed in `PRICING` because their shoppers could not get a shipping quote and gave up. No saga started for them: nothing was reserved or charged, and the sessions expire like any abandoned checkout.

## NATS outage: outbox catch-up

With NATS down for 20 s, there were no shopper errors and checkouts carried on. Every service writes its events to its own outbox in the business transaction (ADR-006), so a broker outage only delays events.

| | Run 1 | Run 2 |
|---|---|---|
| Checkouts during the drill | 2,259 | 1,103 |
| Outbox peak (all services) | 8,443 rows | 952 rows |
| Outbox drained after NATS returned | 5 s | 8 s |
| Consumers caught up after traffic stopped | 8 s | 0 s |

NATS came back in 5 s. The relays reconnected and published the backlog in batches, and the durable consumers caught up. One durable consumer, `yc-product-cart-item-added`, belongs to a subscription yc-product no longer has. It never drains, so the check ignores idle consumers (v2 backlog: clean up orphan consumers).

## Gateway restart

A killed gateway serves its existing routes again within 2 s of starting, from the routes services re-register every 5 s. `/readyz` stays unready for `registry.startup_grace` (10 s), so a load balancer only sends traffic once every service has had two chances to register. Before Phase 9 the interval was 30 s and a restarted gateway answered `404` for up to 30 s (load test report). In kill-gateway, the gateway was ready 10 s after starting, and the 8 shopper errors were all connection errors during the outage itself.

## Graceful shutdown

`docker stop` on all 17 services exited 0 in 0.1–0.3 s each, with no errors logged. On SIGTERM a service reports `/readyz` as `draining` for `http.shutdown_delay`, stops accepting connections, finishes in-flight requests within `http.shutdown_timeout` (20 s), then closes its consumers and relay (10 s) and flushes telemetry (5 s). Compose allows 40 s (`stop_grace_period`) before SIGKILL; ADR-026 rule 4 sets the rule for other targets. The images have a `HEALTHCHECK` (`/app/service healthcheck`), and all 17 containers report healthy.

## Findings

1. **An idempotency key can be stuck "in progress" after a crash.** Seen in run 1's kill-orders drill. If a service dies after committing a request but before storing its response, the key's lease is never released. Retries with the same key get `409` until the 2-minute lease lapses, then replay safely. The saga itself recovers. v2 backlog: record the idempotency result in the business transaction.
2. **Public routes of a deregistered service return `404`.** A service that fails health checks is deregistered, and requests to its paths return `404` until it re-registers. A client can't tell that from "no such endpoint", and won't retry. The internal listener already returns `503` (ADR-025 rule 18). v2 backlog: return `503` on the public listener too, for path prefixes of known services.
3. **The chaos script could leave Postgres frozen.** In run 1 the pause-postgres drill queried Postgres before unfreezing it, and the script exited on the error with Postgres still paused. The script now restores whatever a drill broke on any exit, and only queries Postgres once it's back.
4. **A sleeping Mac invalidates a run.** Run 1's kill-orders drill failed with one session in `CONFIRMING`. The Mac slept for 8 minutes during the drill: Docker froze but the settle timer didn't, so the check ran out of time. When the machine woke, the sweeper completed the session within 10 s. Both test scripts now hold `caffeinate` while they run. Run 2 repeated the drill and it passed.

## Runs

| Run | Images | Result |
|---|---|---|
| `1792063600` | Before the deployment-prep changes | 11 passed; kill-orders invalid (sleep); pause-postgres stopped by the script bug |
| `1792066214` | Final (5 s registration, startup grace, healthchecks, 40 s stop grace) | 13 of 13 passed |
