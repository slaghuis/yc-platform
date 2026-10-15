# ADR-027: Frontend Applications and Edge Topology

**Status:** Proposed
**Date:** 2026-10-15
**Context:** v1 ships APIs only. The next step is two browser applications in their own repositories: `yc-platform-admin` (marketing site and system admin console on `yellowcommerce.co.za`) and `yc-platform-shop` (a demonstration shop with storefront and shop admin on `yellowcommercedemo.co.za`). How a browser reaches the platform is not decided, and v1 has several properties that rule out the obvious answer of "the browser calls yc-gateway":

1. **The gateway owns root paths on every host.** Public routes are `/products`, `/cart`, `/orders/{id}`, `/login`, `/shipping/tracking/{n}` and so on (api-conventions §3). A storefront on the same host needs those paths for pages.
2. **The platform already links to storefront pages on those paths.** yc-message builds `{shop}/orders/{id}`, `{shop}/cart`, `{shop}/products/{id}#review` and `{shop}/shipping/tracking/{n}`; yc-identity builds `/account/reset-password?token=…`. Today those links land on JSON.
3. **Tokens are returned in the response body** (`POST /login`, `POST /refresh`) and sent as `Authorization: Bearer`. A browser calling the gateway directly would hold access and refresh tokens in JavaScript.
4. **CORS is not implemented** (`yc-gateway.md`, Future Scope), and api-conventions §5 forbids 3xx, so the gateway is not a browser-facing server.
5. **Tenant resolution is by hostname only** (ADR-020). A token must belong to the host's tenant, so system admins work on the platform host and shop admins on their shop's host.

## Decision

**Each frontend is a server-rendered application with its own server side (a backend-for-frontend, BFF). Browsers talk only to their frontend; the frontend's server calls yc-gateway over the private network, presenting the browser's hostname. A public edge proxy terminates TLS, sends each host to its frontend, and exposes yc-gateway only for `/webhooks/` on shop hosts.**

```
                 ┌──────────── public edge (TLS) ─────────────┐
browser ──https──▶ yellowcommerce.co.za      → yc-platform-admin │
                 │ yellowcommercedemo.co.za  → yc-platform-shop  │
                 │   …/webhooks/*            → yc-gateway :8080  │
                 └─────────────────────────────────────────────┘
yc-platform-admin / yc-platform-shop ──http, Host: <browser host>──▶ yc-gateway :8080 (private)
```

## Rules

### Edge

1. **Routing.** The edge routes by hostname. Platform hosts go to yc-platform-admin. A shop's hostnames go to that shop's frontend, except `/webhooks/*`, which goes to yc-gateway's public listener. No other gateway path is reachable from the internet.
2. **Headers.** The edge sets `X-Forwarded-For` and `X-Forwarded-Proto`, and forwards `Host` unchanged.
3. **Trusted proxies.** yc-gateway's `trusted_proxies` lists the edge and every frontend server, so rate limits apply to the visitor's address (ADR-026 rule 7), not to the frontend's.

### Frontend to gateway

4. **Host is the browser's host.** The BFF calls the gateway's private URL with `Host` set to the hostname the browser used (with no port). Tenant resolution, platform-host rules, password-reset links (`{host}`) and OAuth redirects (`{host}`) then work unchanged.
5. **Forwarded client address.** The BFF appends the browser's address to `X-Forwarded-For` (the address it got from the edge) and passes `Accept-Language`, `X-YC-Display-Currency`, `X-YC-Correlation-ID` (when it has one) and `Idempotency-Key`.
6. **No direct browser calls.** Browser code never calls yc-gateway. Client components call their own frontend's route handlers or server actions, which call the gateway.

### Sessions and tokens

7. **Tokens stay on the server.** After `POST /login` (or the OAuth callback), the BFF keeps the access token, refresh token and expiry in a sealed session cookie: encrypted and signed with a server secret, `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`. The browser never sees a token. A session cookie larger than 4 KB is a defect, not a reason to add a session store.
8. **Refresh.** The BFF refreshes when the access token is within 60 s of expiry, or once after a `401`, and rewrites the cookie. A failed refresh (a replaced or revoked family, ADR-021 / yc-identity Phase 3) clears the session and sends the user to sign in.
9. **Logout** calls `POST /logout` with the refresh token, then clears the cookie.
10. **Visitors.** On shop hosts the gateway's `yc_visitor` cookie is relayed both ways unchanged: the BFF forwards the browser's `yc_visitor` to the gateway and passes any `Set-Cookie: yc_visitor` back to the browser. The visitor identity (and so the cart, and the cart merge at login) remains the gateway's. Platform hosts never get one (ADR-020 rule 5).
11. **CSRF.** Mutations reach the BFF only as `POST`/`PUT`/`PATCH`/`DELETE` with a same-origin `Origin` header; the `SameSite=Lax` session cookie is not enough on its own.

### URL ownership on shop hosts

12. **The storefront owns every path on a shop's hostnames** except `/webhooks/`. These pages are a contract with the platform, because platform services generate links to them:

    | Path | Linked from |
    |---|---|
    | `/` | yc-message (`shop_url`) |
    | `/products/{product_id}` (with `#review`) | yc-message review prompt |
    | `/cart` | yc-message abandoned cart |
    | `/orders/{order_id}` | yc-message order emails |
    | `/shipping/tracking/{tracking_number}` | yc-message shipped email |
    | `/account/reset-password?token=…` | yc-identity `password.reset_url` |
    | `/account/accept-invite?token=…` | yc-identity invitations (ADR-030) |
    | `/oauth/{provider}/callback` | OAuth providers (yc-identity `redirect_url`) |
    | `/checkout/{session_id}/return` | Payment gateways (`return_url`, yc-checkout) |

    A platform change that adds or moves a link updates this table and the storefront in the same release.
13. **Platform hosts** follow the same rule: the console owns every path, including `/account/reset-password` and `/account/accept-invite` for system admins.
14. **Links use the shop's primary hostname.** yc-message builds links from the tenant's primary hostname (ADR-029), not from a `{tenant}` template, so shops on their own domains get correct links.

### Assets

15. **Images** are served to browsers from object storage's public URL (`S3_PUBLIC_BASE_URL`), never through a frontend. Shop admin uploads use the presigned flow (`POST /admin/images/upload-url`, browser `PUT` to storage, then `finalize` through the BFF). The bucket's CORS allows `PUT` from shop hostnames only.

### Later

16. **Direct API access** (a mobile app, a third-party storefront) is added as an extra tenant hostname such as `api.<shop host>` routed by the edge to yc-gateway, together with per-tenant CORS in yc-gateway. It needs its own ADR; nothing in this decision blocks it.

## Consequences

**Positive**
- The gateway is no longer internet-facing except for webhooks: one less public attack surface, and the gateway stays a JSON API without CORS or browser concerns.
- Tokens never reach browser JavaScript, so an XSS bug cannot steal a refresh token.
- Every v1 rule (hostname tenancy, platform hosts, visitor cookies, reset and OAuth `{host}` links, rate limits) works without a gateway change beyond configuration.
- The links platform services already send become real pages.

**Negative**
- Every API call takes one extra private hop through the BFF.
- Server-side rendering makes several gateway calls per page under the visitor's quota (120/min, burst 30). Frontends must batch and cache public catalogue data per tenant (yc-platform-shop ADR-003) or the quota is reached by ordinary browsing.
- Each frontend must implement session sealing, refresh and cookie relay correctly; mistakes there are security bugs. Both repos share one design (yc-platform ADR-028) to keep it to one implementation pattern.
- The page-URL contract (rule 12) couples platform releases to the storefront.

## Alternatives Considered

- **Browser calls the gateway on the shop host, pages on the same host**: rejected; API and page paths collide (`/cart`, `/orders/{id}`, `/products`), and tokens would live in JavaScript.
- **Browser calls the gateway on an `api.` subdomain with CORS**: rejected for now; needs CORS in the gateway, puts tokens in JavaScript (or needs a cookie-based token mode in yc-identity), and a SameSite visitor cookie across subdomains complicates the cart. Kept as the route for non-browser clients (rule 16).
- **An `/api/` prefix stripped by the edge**: rejected; still puts tokens in JavaScript, and every service's path, OpenAPI spec and link builder would differ between edge and gateway.
- **A server-side session store (Redis) instead of a sealed cookie**: rejected for v1.1; ADR-026 runs one replica per frontend too, tokens fit in a cookie, and a store is one more stateful component. Revisit with server-side revocation needs.

## Amends

- ADR-026 rule 7: the internet-facing listeners are the edge's; yc-gateway's public listener is reachable from the edge (for `/webhooks/`) and the frontends only.
- `docs/services/yc-message.md`: links come from the tenant's primary hostname (rule 14).
