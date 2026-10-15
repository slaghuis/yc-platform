# Developer Onboarding

- [`developer-setup.md`](developer-setup.md) — Install tools and prepare a workstation
- [`first-time-setup.md`](first-time-setup.md) — Clone, bootstrap, start the dev stack; local service URLs and troubleshooting
- [`frontend-development.md`](frontend-development.md) — The stack for frontend work: dev edge, seed accounts, links and ports
- [`daily-workflow.md`](daily-workflow.md) — Day-to-day commands
- [`adding-a-service.md`](adding-a-service.md) — Scaffold a new service
- [`adding-an-event.md`](adding-an-event.md) — Add a new event schema and wire it through
- [`monorepo-layout-and-tooling.md`](monorepo-layout-and-tooling.md) — Platform reference for coders

## Your first day

1. **Morning: a running stack.** Follow `developer-setup.md` and `first-time-setup.md` until `task devstack:up && task devstack:app` reports every service ready.
2. **Watch it work.** Run `task demo:phase4` (a checkout end to end) and `task demo:phase8` (monitoring, audit, GDPR). Each prints what it does; open MailHog and Jaeger (URLs in `first-time-setup.md`) to see the messages and traces.
3. **Read the shape of the system.** ADR-001 (tenants), ADR-006 (outbox), ADR-017 (database per service), ADR-019 (checkout saga), ADR-020 and ADR-021 (gateway and internal calls), ADR-026 (how it is deployed). Then `services/yc-checkout.md`, which touches most other services.
4. **Afternoon: a small change.** Read `daily-workflow.md` and the standards (`docs/standards/`), pick an item from `docs/backlog-v2.md` or a doc fix, and take it through lint, tests and a pull request.
5. **Know where to look when it breaks.** Skim `docs/runbooks/README.md` and the Phase 9 reports (`docs/reports/`).
