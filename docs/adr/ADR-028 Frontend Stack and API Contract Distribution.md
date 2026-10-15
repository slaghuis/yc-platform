# ADR-028: Frontend Stack and API Contract Distribution

**Status:** Accepted
**Date:** 2026-10-15
**Context:** Two frontend repositories start at once (`yc-platform-admin`, `yc-platform-shop`), both built mostly by the AI code factory against the same gateway. ADR-027 requires each to be a server-rendered application with a backend-for-frontend that holds tokens in a sealed cookie, relays the visitor cookie and forwards the browser's host. If the two repos choose different stacks, that security-sensitive layer is written twice in two idioms. The frontends also need the platform's API types: the source of truth is each service's `api/openapi.yaml` (api-conventions §2), which lives in this repository. yc-platform is a Go workspace with a Go-only pipeline (ADR-018); `pipeline-lib` has no Node support.

## Decision

**Both frontends use the same stack: Next.js (App Router) with React and TypeScript on Node. Each frontend vendors the OpenAPI specs of a tagged yc-platform release and generates its own types and client from them. yc-platform publishes no JavaScript package.**

## Rules

### Stack

1. **Framework.** Next.js with the App Router, React server components and TypeScript in `strict` mode. Node is the current LTS, pinned in `.nvmrc` and `package.json` `engines`. Package manager: pnpm, with the lockfile committed.
2. **UI.** Tailwind CSS and shadcn/ui components (copied into the repo, not a runtime dependency). Forms use react-hook-form with Zod schemas. Admin tables use TanStack Table.
3. **Server boundary.** Gateway calls happen only in server code (server components, server actions, route handlers) in a `server/` module that client components cannot import (`import "server-only"`). That module owns the session cookie, refresh, visitor relay and header forwarding (ADR-027 rules 4–11).
4. **Errors.** The gateway's Problem+JSON (api-conventions §6) is parsed into one typed error. Pages show the `title`, field errors next to their fields, and the `correlation_id` on unexpected failures so support can find the trace.
5. **Money and time.** Amounts stay in minor units with their currency until rendered, and are formatted with `Intl.NumberFormat` (api-conventions §13). No floating-point arithmetic on money in the frontend.
6. **Tests.** Vitest for units, Playwright for end-to-end tests against the yc-platform compose stack (`task devstack:up` plus the services). End-to-end tests are the milestone checks in each repo's implementation plan.
7. **Quality gates.** ESLint (Next.js and TypeScript rules), Prettier, `tsc --noEmit`, unit tests, a production build, and `pnpm audit --prod` at high severity, run by GitHub Actions in each repository. Extending `pipeline-lib` with a Node mode is backlog work, not a prerequisite.
8. **Image.** `output: "standalone"`, run as a non-root user on a distroless Node image, listening on `:3000`, configured only through environment variables, exiting cleanly on `SIGTERM`. `GET /healthz` (process up) and `GET /readyz` (the gateway's `/readyz` answers) follow ADR-026 rules 2–4 in spirit. One replica, as for every service.

### API contract

9. **Pinned to a platform tag.** Each frontend has `api/platform-version` naming a yc-platform tag (`v1.1.0`), and `api/specs/<service>.yaml` holding the copied specs of the services it uses. `scripts/sync-api.sh <tag>` fetches them with `git archive` from the tag and regenerates types.
10. **Generated code.** Types come from openapi-typescript into `src/server/api/generated/` and calls go through openapi-fetch. Both the specs and the generated files are committed, so an API change shows up as a reviewable diff in the frontend.
11. **Drift check.** CI regenerates from `api/specs/` and fails if the generated files differ.
12. **yc-platform tags releases.** From v1.1 onwards, every platform release is a semver git tag. A change that a frontend must follow (a new required field, a moved link per ADR-027 rule 12) is called out in the release notes. So is every new, changed or removed system admin capability: a `/sysadmin/*` route or a route that admits `system_admin`, an alert rule, a runbook or an operator tool. yc-platform-admin maps each of these to a console requirement or an exclusion (its `docs/platform-coverage.md`) and updates that map when it moves to the tag.

## Consequences

**Positive**
- One BFF and session pattern, reviewed once, in two repos.
- Server components keep tokens and most data fetching on the server, which ADR-027 requires anyway; catalogue pages render server-side for search engines.
- yc-platform stays a Go-only workspace; no registry, npm token or Node toolchain is added to it.
- API changes reach the frontends deliberately, at a tag, with a diff.

**Negative**
- The specs are copied into two repos; a fix in yc-platform reaches the frontends only after a tag and a sync.
- The repos use GitHub Actions instead of `pipeline-mcp`, so agents cannot run their checks through `pipeline_run` until `pipeline-lib` supports Node.
- Next.js upgrades (App Router, caching semantics) are a recurring maintenance cost.
- The BFF code is duplicated between the repos. If it diverges or a third frontend appears, extract it into a shared package.

## Alternatives Considered

- **Single-page apps (React with Vite) calling a thin BFF**: rejected; the storefront needs server rendering for search engines and first paint, and two runtimes per app is more to deploy.
- **SvelteKit or Remix**: both fit ADR-027; rejected in favour of the largest ecosystem and the most AI-assisted tooling familiarity, which matters for the code factory.
- **Publish `@yc/api-client` from yc-platform**: rejected for now; it adds Node, a registry and publishing credentials to a Go workspace for two consumers. Revisit for a third frontend or the shared BFF package.
- **Generate a Go BFF from the specs (oapi-codegen) and serve templates**: rejected; a rich shop admin needs a component ecosystem the Go template world lacks.
