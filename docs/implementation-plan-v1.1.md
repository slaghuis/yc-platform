# Implementation Plan — yc-platform v1.1 (Frontend Enablement)

v1 (`implementation-plan.md`, Phases 0–9) delivered the platform as APIs only. v1.1 adds what two browser applications need from the platform, and sequences the platform work against them:

| Repository | Hosts | What it is |
|---|---|---|
| `yc-platform` (this repo) | — | The platform. v1.1 is Phases 10–13 below. |
| `yc-platform-admin` | `yellowcommerce.co.za` | Public marketing site and the system admin console |
| `yc-platform-shop` | `yellowcommercedemo.co.za` | Demonstration shop for a fictitious customer: branded storefront, and the shop admin for shop admins |

Each frontend repository has its own `docs/implementation-plan.md`; this document owns the platform phases and the cross-repository schedule.

The decisions are ADR-027 (edge topology and backend-for-frontend), ADR-028 (frontend stack and API contract), ADR-029 (tenant registry), ADR-030 (admin invitations and platform team) and ADR-031 (demo hosting). All five are **Proposed**; Phase 10 starts by accepting or amending them.

---

## Guiding Principles

The v1 principles still apply. In addition:

1. **The platform leads by one phase.** A frontend phase never waits on platform code that is still being designed; it builds against a tagged release (ADR-028 rule 9), or against v1 behaviour where that is enough (for example register-then-promote for shop admins in development).
2. **Browsers never touch the gateway** (ADR-027). Every platform change in v1.1 is judged by what the backend-for-frontend needs, not what a browser needs.
3. **The storefront URL contract is part of the API** (ADR-027 rule 12). Moving a link is a breaking change.
4. **Demo, not production.** v1.1 ends with a public demonstration environment. Production hosting is a later ADR.

---

## Phase Overview

| Phase | Duration (indicative) | Milestone |
|---|---|---|
| **10. Frontend enablement** | 1 week | Both frontends run locally behind a dev edge against the compose stack; `v1.1.0-alpha.1` tagged |
| **11. Tenant registry** | 2 weeks | Shops are created, listed, renamed, re-hosted, suspended and deleted through `/sysadmin/tenants` |
| **12. Invitations and platform team** | 1.5 weeks | A system admin invites a colleague and a shop owner; both accept from an email link |
| **13. Demo environment** | 1 week | `yellowcommerce.co.za` and `yellowcommercedemo.co.za` are live on HTTPS; `v1.1.0` tagged |

Platform total: about **5.5 weeks**. The frontends run in parallel (schedule below), so v1.1 as a whole is about **10–12 weeks** to a public demo.

---

## Cross-Repository Schedule

Weeks are indicative and assume one engineer per stream with the AI code factory. `A` phases are yc-platform-admin, `S` phases yc-platform-shop.

| Week | yc-platform | yc-platform-admin | yc-platform-shop |
|---|---|---|---|
| 1 | 10. Frontend enablement | A0. Foundations | S0. Foundations |
| 2 | 11. Tenant registry | A1. Marketing site | S1. Browse |
| 3 | 11. Tenant registry | A2. Console shell and auth | S2. Cart and accounts |
| 4 | 12. Invitations | A3. Platform health | S3. Checkout |
| 5 | 12. Invitations | A4. Shops ⟵ needs 11 | S3. Checkout |
| 6 | 13. Demo environment | A5. Team and users ⟵ needs 12 | S4. After the purchase |
| 7 | (support, backlog) | A6. Activity and audit | S5. Shop admin: catalogue |
| 8 | | A7. Release ⟵ needs 13 | S6. Shop admin: operations |
| 9 | | | S7. Shop admin: settings and engagement |
| 10 | | | S8. Brand, demo content and release ⟵ needs 13 |

Hard dependencies:

| Frontend phase | Needs | Why |
|---|---|---|
| A0, S0 | Phase 10 | Dev edge, trusted proxies, dev hostnames, a platform tag to pin |
| A4 Shops | Phase 11 | `/sysadmin/tenants` |
| A5 Team and users | Phase 12 | `/sysadmin/invitations`, suspend, demote |
| S2 accept-invite page, A2 accept-invite page | Phase 12 | `POST /invitations/accept` (the pages can be built against the contract earlier) |
| S5 Shop admin | none | In development, shop admins are created with v1 register-then-promote |
| A7, S8 Release | Phase 13 | The environment |

---

## Phase 10 — Frontend Enablement (1 week)

Make the platform consumable by a backend-for-frontend, locally, with no new services.

### Scope

- Accept or amend ADR-027 to ADR-031; record the outcome in `docs/adr/README.md`
- Merge Phase 9, tag `v1.0.0` on `main`, and start semver tags (ADR-028 rule 12)
- **Dev edge:** a `frontends` Compose profile with Caddy on `:8080` routing `admin.localhost` to the console (default `host.docker.internal:3000`) and `yc-demo.localhost` to the shop (`host.docker.internal:3001`), with `/webhooks/*` on shop hosts to yc-gateway (ADR-027 rules 1–2)
- **yc-gateway configuration:** dev `trusted_proxies` covering the edge and the host's Docker bridge address; a test proving that a request forwarded by a trusted proxy is rate-limited by the browser's address
- **Dev tenant:** a new `task devstack:seed` provisions `yc-demo` with `yc-demo.localhost` (the frontends' development shop), creates the bootstrap system admin, and a shop admin for `yc-demo` by register-then-promote
- **Links to storefront pages:** dev `YC_IDENTITY__PASSWORD__RESET_URL` and OAuth `redirect_url` pointing at the edge (`http://{host}:8080/...`), yc-message `shop_url` at `http://{tenant}.localhost:8080` until Phase 11 replaces it
- **Object storage CORS** for presigned `PUT` from shop hostnames (ADR-027 rule 15)
- **Storefront URL contract** documented in `docs/standards/api-conventions.md` (new section, pointing at ADR-027 rule 12), and a test in yc-message asserting the link paths it builds
- `docs/onboarding/frontend-development.md`: running the stack for frontend work, dev hostnames, seeding, the bootstrap system admin, creating a dev shop admin

### Not in scope

- yc-tenant, invitations, hosting
- Any frontend code (that is A0 and S0)

### Milestone

```
task devstack:up && task devstack:app && task devstack:seed
docker compose --profile frontends up -d                          # the dev edge on :8080
curl -H 'Host: yc-demo.localhost' localhost:8090/products        # catalogue through the gateway
curl -H 'Host: admin.localhost' -X POST localhost:8090/login ...  # system admin token
git tag v1.1.0-alpha.1
```

### Exit criteria

- The dev edge routes both hostnames and webhooks as ADR-027 describes
- A password-reset email in MailHog links to `http://yc-demo.localhost:8080/account/reset-password?token=…`
- `v1.0.0` and `v1.1.0-alpha.1` tags exist; the frontends can pin the latter

---

## Phase 11 — Tenant Registry (2 weeks)

ADR-029.

### Scope

- **yc-tenant** scaffolded with `scripts/gen-service.sh`: `yc_tenant` database, `tenants` and `tenant_hostnames`, outbox, health, registration with the gateway, image, compose entry
- `/sysadmin/tenants` API and OpenAPI spec (ADR-029 rule 7); hostname uniqueness checks against all tenants and `platform_hosts`
- **Schemas:** `platform.tenant.updated.v1`, `.suspended.v1`, `.reinstated.v1`, `.deleted.v1`; the event taxonomy updated
- **yc-gateway:** handle `updated` (replace a tenant's hostnames), `suspended` (503 `tenant_suspended` on every route including webhooks), `reinstated`, `deleted` (410, hostnames freed); the suspended and deleted sets persisted in `yc_gateway` and loaded at startup
- **yc-message:** tenant projection (name, primary hostname); `shop_url` and all links built from it, falling back to `messages.shop_url`
- **yc-analytics:** tenant name changes into `dim_tenant`
- **yc-audit:** the new lifecycle events as `tenant_lifecycle`
- **yc-tenant-cli:** `provision` calls the API with `YC_TENANT_CLI_TOKEN`; the direct NATS publish is removed
- **`yc-tenant import`:** one-off import of existing tenants from yc-gateway and yc-pricing
- Runbooks: `onboarding-tenant.md` rewritten around the API, new `suspending-a-tenant.md`

### Not in scope

- Purging a deleted tenant's data across services (backlog-v2 P2)
- Per-shop checkout configuration, CORS, branding fields

### Milestone

1. A system admin creates `acme` with `acme.localhost` through `POST /sysadmin/tenants`; the storefront host answers within seconds
2. Renames it and adds `www.acme.localhost` as primary; an order email links to the new primary host
3. Suspends it; every request on its hosts answers 503 `tenant_suspended`, including after a gateway restart
4. Reinstates it; traffic resumes
5. Suspends and deletes it; hosts answer 410 and `acme.localhost` can be given to a new tenant
6. yc-audit shows all five lifecycle records with the acting system admin

### Exit criteria

- Contract tests for the new API; integration tests for each gateway lifecycle handler
- `scripts/chaos-test.sh` passes (the gateway changed; ADR-026 rule 9)
- `v1.1.0-alpha.2` tagged

---

## Phase 12 — Invitations and Platform Team (1.5 weeks)

ADR-030.

### Scope

- **yc-identity:** invitations table and migration; `/sysadmin/invitations` (create, list, resend, revoke); anonymous `POST /invitations/accept` with login-style throttling; existing-customer upgrade; consuming `platform.tenant.provisioned.v1` to invite `primary_admin_email`
- **yc-identity:** `suspend`, `reactivate`, `demote` (ADR-030 rules 8–10), including the self and last-system-admin guards
- **Schemas:** `identity.user.invited.v1`, `.invitation_accepted.v1`, `.invitation_revoked.v1`, `.suspended.v1`, `.reactivated.v1`, `.demoted.v1`
- **yc-message:** `admin_invite` platform-default template (email); admin audience projection from accepted invitations, demotions and suspensions
- **yc-audit:** the six events; **yc-preference:** defaults on `invitation_accepted`
- **yc-gateway:** static anonymous route for `/invitations/accept`
- `docs/services/yc-identity.md` and `runbooks/onboarding-tenant.md` updated (step 3 becomes "the owner accepts the invitation")

### Not in scope

- Shop admins inviting shop admins; MFA; SSO

### Milestone

1. A system admin invites a colleague as `system_admin`; the email in MailHog links to `admin.localhost`; the colleague sets a password and logs in to the console
2. A system admin creates shop `acme` with `primary_admin_email`; the owner receives an invite on `acme.localhost`, accepts it, and reaches the shop admin
3. An existing customer of `acme` is invited and keeps their order history after accepting
4. Suspending the colleague ends their session within 60 s; suspending yourself or the last system admin is refused
5. Monitor alerts for `acme` reach the new shop admin

### Exit criteria

- Token single-use, expiry and enumeration-resistance covered by tests
- `v1.1.0-beta.1` tagged

---

## Phase 13 — Demo Environment (1 week)

ADR-031.

### Scope

- Provider chosen (ADR-031 Open Questions); VM provisioned and hardened (key-only SSH, firewall, unattended security updates)
- `deploy/demo/`: Compose override, Caddyfile, env template, deploy script, backup job
- DNS and TLS for `yellowcommerce.co.za`, `yellowcommercedemo.co.za` and their `www` hosts
- Bootstrap system admin; `yc-demo` provisioned through yc-tenant with its owner invited
- Stripe and Yoco test-mode configuration and webhooks
- `runbooks/rotate-secrets.md` and `runbooks/demo-environment.md` (deploy, restore, rebuild)
- `pipeline.yaml` `deploy` section replaced or removed (backlog-v2 P1)
- Restore drill from a nightly backup

### Milestone

- Both domains serve their frontends over HTTPS; a test-mode Yoco checkout on the demo shop completes, its webhook arrives, and the confirmation email is delivered
- `v1.1.0` tagged; both frontends pinned to it

### Exit criteria

- `task load` and `scripts/chaos-test.sh` pass against a stack built from `v1.1.0` (ADR-026 rule 9)
- Only ports 80 and 443 are reachable from the internet

---

## Backlog Items Taken Into v1.1

| backlog-v2 item | Where |
|---|---|
| Admin invite email at provisioning (P1) | Phase 12 |
| `pipeline.yaml` deploy section (P1) | Phase 13 |
| `rotate-secrets.md` runbook (P1) | Phase 13 |
| Tenant lifecycle subscriptions (P2), gateway and yc-message only | Phase 11 |
| Clients poll the session after a failed confirm (P2) | yc-platform-shop S3 |

Everything else stays in `backlog-v2.md`. The service-token-for-route-registration finding (P1) remains a deployment requirement: ADR-031 keeps the internal listener on the Compose network.

---

## Open Decisions

| Decision | Needed by | Owner |
|---|---|---|
| Accept, amend or reject ADR-027 to ADR-031 | Start of Phase 10 | Platform owner |
| The fictitious customer: name, product range, brand | S1 (placeholder brand until then) | Product owner |
| Hosting provider and mail provider (ADR-031) | Start of Phase 13 | Platform owner |
| Product marketing beyond what v1 supports (promotions, campaigns) | After v1.1; see yc-platform-shop requirements | Product owner |
| GitHub repositories and visibility for the two frontends | Phase 10 | Platform owner |
