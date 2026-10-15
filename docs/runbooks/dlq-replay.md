# DLQ Replay

A consumer that keeps failing on an event gives up after its retries and moves the event to the tenant's DLQ subject, `yc.<tenant>.<domain>.dlq.v1`, in the `YC_DLQ` stream (ADR-003). yc-monitor records every dead letter, alerts when more than 10 arrive in a clock minute (`dlq_rate`), and `yc-dlq-cli` replays them (ADR-025).

## When to use

- yc-monitor raised a `dlq_rate` alert, or the system admin dashboard shows unacknowledged dead letters.
- You've found why the consumer failed and fixed it (a service is back up, a bug fix is deployed), or confirmed the event was poison and should not be replayed.

## Procedure

1. **See what is failing.** As a system admin, through the gateway's admin host:
   ```bash
   curl -b cookies.txt http://admin.localhost:8090/sysadmin/dlq/summary
   curl -b cookies.txt "http://admin.localhost:8090/sysadmin/dlq?tenant_id=<tenant>&acknowledged=false&limit=50"
   ```
   The summary counts unacknowledged dead letters per tenant and service; the list shows each one's `dlq_sequence`, consumer, event type and `failure_reason`. A shop admin sees their own shop's at `GET /admin/dlq`.
2. **Find the cause** in the consuming service's logs (the `service` and `consumer` fields), and fix it. Typical causes: a service it calls was down, or a bug in the handler. Do not replay until the cause is fixed — the event will just dead-letter again.
3. **List them with yc-dlq-cli.** It reads `YC_DLQ` directly (`--nats-url`, default `$NATS_URL` or `nats://localhost:4222`):
   ```bash
   go run ./tools/yc-dlq-cli list --tenant <tenant> --domain <domain> --service <service>
   go run ./tools/yc-dlq-cli show --seq <dlq_sequence>
   ```
   Filters: `--tenant`, `--domain`, `--service`, `--consumer`, `--event-type`, `--event-id`, `--seq` (repeatable or comma-separated). `list` shows the newest 50 (`--limit`, `--json` for one object per line).
4. **Replay**, dry run first:
   ```bash
   go run ./tools/yc-dlq-cli replay --tenant <tenant> --domain <domain> --service <service> --all --dry-run
   go run ./tools/yc-dlq-cli replay --tenant <tenant> --domain <domain> --service <service> --all
   ```
   `replay` needs `--seq`, `--event-id` or `--all` (with the other filters). It republishes the original event, unchanged, to its original subject, with a `YC-Replay-Of` header naming the DLQ sequence. Consumers that already processed the event skip it, so in effect only the consumer that gave up runs again. Dead letters from a tenant mismatch (the `_platform` DLQ) are never replayed.
5. **Verify** the downstream effect in the consuming service (for example the emails in MailHog for yc-message).
6. **Acknowledge** each dead letter so it leaves the summary and dashboard:
   ```bash
   curl -b cookies.txt -X POST http://admin.localhost:8090/sysadmin/dlq/<dlq_sequence>/ack
   ```
   Acknowledge poison events you decided not to replay too, and note why in the incident log.

Replays can trigger side effects (emails, charges). Replay only after human review (`docs/standards/event-conventions.md` §20). `scripts/demo-phase8.sh` (step 5) walks through the whole procedure.
