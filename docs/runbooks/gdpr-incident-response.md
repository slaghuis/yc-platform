# GDPR Cascade Incident

## When to use

yc-audit raised `audit.gdpr.cascade_incident.v1` — a user deletion did not complete within 72 hours (ADR-005, ADR-025). yc-monitor opens a critical `gdpr_cascade_incident` alert and yc-message emails the system admins.

## Procedure

0. **Triage.** Log in as a system admin (the gateway takes the access token only as a bearer header), then list the incidents and the register:
   ```bash
   TOKEN=$(curl -fsS -X POST http://admin.localhost:8090/login -H 'Content-Type: application/json' \
     -d '{"email":"<email>","password":"<password>"}' | jq -r .access_token)
   curl -H "Authorization: Bearer $TOKEN" http://admin.localhost:8090/sysadmin/gdpr/incidents
   curl -H "Authorization: Bearer $TOKEN" "http://admin.localhost:8090/sysadmin/gdpr/register?status=in_progress"
   ```
   From v1.1 the yc-platform-admin console shows the same under GDPR.
   One user with one missing service is usually that service's consumer. Many users missing the same service means the service is down or its handler is broken. Many users missing many services points at the event bus or yc-identity.
1. **Identify the lagging services** for a user:
   ```bash
   curl -H "Authorization: Bearer $TOKEN" http://admin.localhost:8090/sysadmin/gdpr/register/<user_id>
   ```
   The entry lists `expected_services`, `missing_services` and each service's report (action and count). The shapes are in [`docs/services/yc-audit.md`](../services/yc-audit.md).
2. **For each lagging service**, check its logs for its GDPR consumer (`<service>-gdpr`, subscribed to `identity.user.deleted.v1`).
3. If the service is down, bring it up — the durable consumer catches up on its own.
4. If the service is up but the handler failed, it gave up and dead-lettered the event: check `GET /sysadmin/dlq?service=<service>` and follow [`dlq-replay.md`](dlq-replay.md) once the cause is fixed.
5. Confirm the register entry is `complete` (step 1; it is `incident` while overdue). yc-audit then publishes `audit.gdpr.cascade_complete.v1`; the incident stays listed under `/sysadmin/gdpr/incidents` as completed late.
6. **Acknowledge** the `gdpr_cascade_incident` alert (`POST /sysadmin/alerts/<id>/ack`); incident alerts resolve when acknowledged.
7. If the deadline cannot be met, log the exception, notify the data protection officer, and document it in the incident log.

## To expand

- **Communication.** Name the DPO role and the escalation path, and state which internal notifications are required.
- **Manual completion.** Describe how to mark a cascade entry complete by hand when a service cannot process the event — only after the data has actually been anonymized or deleted. There is no endpoint for this yet.
