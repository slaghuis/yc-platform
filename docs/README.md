# Documentation

- `adr/` — Architecture Decision Records
- `services/` — Per-service requirement specifications
- `standards/` — Coding, API, event, logging, testing standards
- `runbooks/` — Operational procedures
- `onboarding/` — Developer onboarding and workflows
- `reports/` — Phase 9 hardening reports: load test, security review, chaos test
- `diagrams/` — Architecture diagrams, including `event-taxonomy.md`, the registry of every event in the platform
- `implementation-plan.md` — Phased build plan for v1 (Phases 0–9, as built)
- `implementation-plan-v1.1.md` — v1.1 frontend enablement (Phases 10–13) and the schedule across `yc-platform-admin` and `yc-platform-shop`
- `backlog-v2.md` — Work deferred from v1, ranked

New to the platform? Start with `onboarding/README.md`. Before running it anywhere but a laptop, read ADR-026 (the deployment contract) and the deployment requirements in `reports/security-review.md`. Browsable API docs: `task docs:api`, then open `dist/api/index.html`.
