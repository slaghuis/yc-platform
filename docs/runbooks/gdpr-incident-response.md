# GDPR Cascade Incident

## When to use

yc-audit raised `audit.gdpr.cascade_incident.v1` — a user deletion did not
complete within 72 hours.

## Procedure

1. Identify the user and the lagging service(s):
   ```bash
   curl /sysadmin/gdpr/register | jq '.incidents'
2. For each lagging service: check its logs for the subscription handler of identity.user.deleted.v1.
3. If the service is down, bring it up — the subscription will catch up.
4. If the service is up but the handler is failing, check the DLQ for that service and follow dlq-replay.md.
5. Once the service completes, confirm: `curl /sysadmin/gdpr/register/<user_id>`
6. If the deadline cannot be met, log the exception, notify the data protection officer, and document in the incident log.
`
That's the runbook as it was produced in the scaffold. A few things worth flagging that we may want to expand on later as the platform matures:

1. **Investigation order**: the current procedure jumps straight to service logs. In practice, you'd probably want a step 0 that triages: is this one user, one service, or systemic? A systemic cascade failure (e.g., yc-eventbus broken, affecting all users being deleted) has a different response than a single stuck user.

2. **Communication requirements**: GDPR has reporting obligations (72h to supervisory authority for breaches; this isn't a breach but may need internal notification). The runbook should probably name the DPO or role and set an escalation path.

3. **Manual completion fallback**: if a service genuinely can't delete the data (bug, migration gone wrong), the runbook should describe how to manually complete the cascade entry so the register is accurate — but only after the deletion is actually done.

4. **Register schema**: the `curl` commands reference endpoints (`/sysadmin/gdpr/register`, `/sysadmin/gdpr/register/<user_id>`) that are defined in the yc-audit service spec. The runbook should link to that spec so operators can see the response shape.

These are observations for when yc-audit is built in Phase 8 — the runbook should be revisited then with the real endpoints and real experience.
`

