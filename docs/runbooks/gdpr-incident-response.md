# GDPR Cascade Incident

*Stub. Revisit when yc-audit is built in Phase 8, with the real endpoints.*

## When to use

yc-audit raised `audit.gdpr.cascade_incident.v1` — a user deletion did not complete within 72 hours (ADR-005).

## Procedure

1. Identify the user and the lagging service(s):
   ```bash
   curl /sysadmin/gdpr/incidents
   ```
   The response shape is defined in [`docs/services/yc-audit.md`](../services/yc-audit.md).
2. For each lagging service, check its logs for the `identity.user.deleted.v1` subscription handler.
3. If the service is down, bring it up — the subscription will catch up.
4. If the service is up but the handler is failing, check the DLQ for that service and follow [`dlq-replay.md`](dlq-replay.md).
5. Once the service completes, confirm the register shows every expected service as complete:
   ```bash
   curl /sysadmin/gdpr/register/<user_id>
   ```
6. If the deadline cannot be met, log the exception, notify the data protection officer, and document it in the incident log.

## To expand

- **Triage first.** Add a step 0: is this one user, one service, or systemic? A systemic failure (for example yc-eventbus broken for every deletion) needs a different response from a single stuck user.
- **Communication.** Name the DPO role and the escalation path, and state which internal notifications are required.
- **Manual completion.** Describe how to mark a cascade entry complete by hand when a service cannot process the event — only after the data has actually been anonymized or deleted.
