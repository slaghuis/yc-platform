# DLQ Replay

*Stub. Full implementation lands with yc-dlq-cli in Phase 1.*

## When to use

- yc-monitor raised an alert for a tenant/domain DLQ.
- You've investigated the failure and have a fix deployed (or confirmed the original message was poison).

## Procedure

1. Identify affected messages:
   ```bash
   yc-dlq-cli list --subject "yc.<tenant>.<domain>.dlq.v1"
   ```
2. Inspect one:
   ```bash
   yc-dlq-cli show --event-id <id>
   ```
3. Replay (dry run first):
   ```bash
   yc-dlq-cli replay --event-id <id> --dry-run
   yc-dlq-cli replay --event-id <id>
   ```
4. Verify the downstream effect in the consumer service.

Replays can trigger side effects (emails, charges). Replay only after human review (`docs/standards/event-conventions.md` §20).
