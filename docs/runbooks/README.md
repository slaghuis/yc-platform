# Runbooks

Step-by-step procedures for operational scenarios.

| Runbook | When to use | Status |
|---|---|---|
| [`checkout-stuck-session.md`](checkout-stuck-session.md) | A checkout session is stuck, or yc-checkout logs `alert=critical` | Written in Phase 4 |
| [`dlq-replay.md`](dlq-replay.md) | Messages are landing in a DLQ | Written in Phase 8 (yc-dlq-cli, yc-monitor) |
| [`gdpr-incident-response.md`](gdpr-incident-response.md) | GDPR cascade incomplete after 72h | Written in Phase 8 (yc-audit) |
| [`onboarding-tenant.md`](onboarding-tenant.md) | Provisioning a new tenant | Stub — completed with yc-tenant-cli in Phase 2 |
| `rotate-secrets.md` | Rotating gateway credentials, JWT keys | Not written yet |
| `removing-a-service.md` | Decommissioning a service (including deleting its durable NATS consumers) | Not written yet |

Each runbook is written for operators who may not be the author. Assume nothing; link to supporting docs.
