# Runbooks

Step-by-step procedures for operational scenarios.

| Runbook | When to use | Status |
|---|---|---|
| [`checkout-stuck-session.md`](checkout-stuck-session.md) | A checkout session is stuck, or yc-checkout logs `alert=critical` | Written in Phase 4 |
| [`dlq-replay.md`](dlq-replay.md) | Messages are landing in a DLQ | Written in Phase 8 (yc-dlq-cli, yc-monitor) |
| [`gdpr-incident-response.md`](gdpr-incident-response.md) | GDPR cascade incomplete after 72h | Written in Phase 8 (yc-audit) |
| [`onboarding-tenant.md`](onboarding-tenant.md) | Provisioning a new tenant | Updated in Phase 12 (invitation accept) |
| [`yoco-setup.md`](yoco-setup.md) | Connecting a shop to Yoco, or Yoco checkouts stuck in `CHARGING` | Written in Phase 9 |
| [`demo-environment.md`](demo-environment.md) | HostAfrica demo VM: deploy, restore, rebuild | Written in Phase 13 |
| [`rotate-secrets.md`](rotate-secrets.md) | Rotating JWT keys, service secrets, cipher keys, sessions | Written in Phase 13 |
| [`suspending-a-tenant.md`](suspending-a-tenant.md) | Suspend / reinstate / delete a shop | Written in Phase 11 |
| `removing-a-service.md` | Decommissioning a service (including deleting its durable NATS consumers) | Not written yet |

Each runbook is written for operators who may not be the author. Assume nothing; link to supporting docs.
