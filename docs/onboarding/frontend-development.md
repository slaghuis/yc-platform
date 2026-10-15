# Frontend Development

Running the platform for work on the frontends: `yc-platform-admin` (console, `admin.localhost`) and `yc-platform-shop` (storefront, `yc-demo.localhost`). The design is ADR-027 (edge and backend-for-frontend) and ADR-028 (stack and API contract).

## Start the Stack

```bash
task devstack:up       # Postgres, NATS, SeaweedFS, Jaeger, MailHog
task devstack:app      # the services, yc-gateway on localhost:8090
task devstack:seed     # system admin, yc-demo shop and shop admin, bucket CORS
task devstack:edge     # the dev edge (Caddy) on localhost:8070
```

Then start a frontend from its own repository (`pnpm dev`): the console on port 3000, the shop on port 3001.

| Open | Goes to |
|---|---|
| http://admin.localhost:8070 | the console (`host.docker.internal:3000`) |
| http://yc-demo.localhost:8070 | the shop (`host.docker.internal:3001`) |
| http://yc-demo.localhost:8070/webhooks/… | yc-gateway (payment webhooks) |
| http://localhost:8025 | MailHog: reset links, order emails, alerts |

Every other `*.localhost` host also goes to the shop, so shops provisioned later (`acme.localhost`) work without edge changes. Browsers resolve `*.localhost` to the loopback address on their own; `curl` does too.

## Accounts

`task devstack:seed` creates these, and running it again changes nothing:

| Role | Sign in on | Email | Password |
|---|---|---|---|
| System admin | `admin.localhost` | `sysadmin@yc.test` | `correct-horse-battery` |
| Shop admin of `yc-demo` | `yc-demo.localhost` | `admin@yc-demo.test` | `demo-shop-admin-pass` |

Override them with `SYSADMIN_EMAIL`, `SYSADMIN_PASSWORD`, `SHOP_ADMIN_EMAIL` and `SHOP_ADMIN_PASSWORD`. The shop admin is created the v1 way: registered as a customer on the shop host, then promoted by the system admin. Invitations replace this in Phase 12.

A customer registers on the storefront like any shopper.

## How a Frontend Calls the Platform

- The browser calls only its frontend. The frontend's server calls `http://localhost:8090` (`YC_GATEWAY_URL`) with `Host` set to the browser's host **without the port** (`admin.localhost`, `yc-demo.localhost`).
- Running a frontend without the edge (`http://localhost:3000`) works when it sends a fixed host instead (`YC_PLATFORM_HOST=admin.localhost` for the console).
- Node's built-in `fetch` silently replaces a `Host` header with the URL's host, so the gateway answers for `localhost`. Send gateway calls through `node:http` (yc-platform-admin `src/server/gateway/transport.ts` is a `fetch`-compatible wrapper for openapi-fetch).
- Tokens stay in the frontend's sealed session cookie; the shop relays the gateway's `yc_visitor` cookie both ways (ADR-027 rules 7–10).

## Ports and Links

The edge listens on `YC_EDGE_PORT` (default 8070). The same variable sets the links the platform sends, so they always point at the edge:

- password reset: `http://{host}:8070/account/reset-password?token=…`
- OAuth callbacks: `http://{host}:8070/oauth/{provider}/callback` (the fake providers in development)
- email links (yc-message): `http://{tenant}.localhost:8070/orders/…`, `/cart`, `/products/…#review`, `/shipping/tracking/…`

To use another port, put `YC_EDGE_PORT=<port>` in `.env` at the repository root and recreate the edge, yc-identity and yc-message (`docker compose --profile app --profile frontends up -d`), then run `task devstack:seed` again for the bucket CORS rule. These paths are the storefront URL contract (`docs/standards/api-conventions.md` §21).

## Rate Limits

The gateway trusts every private address range as a proxy in development (`YC_GATEWAY_TRUSTED_PROXIES`), so a request through the edge and a frontend is limited by the browser's address (ADR-027 rule 3): visitors get 120 requests a minute with a burst of 30. Server-rendered pages that make many gateway calls hit that quickly; cache public catalogue data (yc-platform-shop ADR-003). `YC_GATEWAY_RATE_LIMIT=false` turns limits off for a session.

## API Contract

The frontends build against a yc-platform tag, never `main` (ADR-028 rule 9): `scripts/sync-api.sh <tag>` in each frontend copies the specs from that tag. A change a frontend must follow is called out in the release notes (ADR-028 rule 12).

## Troubleshooting

| Symptom | Cause |
|---|---|
| `502` from the edge | The frontend is not running on its port, or the gateway is down (`docker ps`). |
| Nothing on `localhost:8070` | Another program holds the port: set `YC_EDGE_PORT`. |
| `404 Unknown shop` | The host is not mapped: run `task devstack:seed`, or provision the shop. |
| `404 Unknown shop` from a frontend's server although `curl -H 'Host: …'` works | The frontend called the gateway with Node's `fetch`, which drops `Host`; use `node:http`. |
| `401` on `admin.localhost` with a shop admin | Expected: only system admins sign in on the platform host. |
| An upload to storage fails CORS | The page's origin is not a shop host on the edge port, or the seed has not run since the port changed. |
