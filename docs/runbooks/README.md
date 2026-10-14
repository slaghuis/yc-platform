# Runbooks

Step-by-step procedures for operational scenarios.

| Runbook | When to use | Status |
|---|---|---|
| [`dlq-replay.md`](dlq-replay.md) | Messages are landing in a DLQ | Stub — completed with yc-dlq-cli in Phase 1 |
| [`gdpr-incident-response.md`](gdpr-incident-response.md) | GDPR cascade incomplete after 72h | Stub — completed with yc-audit in Phase 8 |
| [`onboarding-tenant.md`](onboarding-tenant.md) | Provisioning a new tenant | Stub — completed with yc-tenant-cli in Phase 2 |
| `rotate-secrets.md` | Rotating gateway credentials, JWT keys | Not written yet |
| `removing-a-service.md` | Decommissioning a service (including deleting its durable NATS consumers) | Not written yet |

Each runbook is written for operators who may not be the author. Assume nothing; link to supporting docs.
