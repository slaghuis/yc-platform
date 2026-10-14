# DLQ Replay

## When to use

- yc-monitor raised an alert for a tenant/domain DLQ.
- You've investigated the failure and have a fix deployed (or confirmed the
  original message was poison).

## Procedure

1. Identify affected messages:
   ```bash
   yc-dlq-cli list --subject "yc.<tenant>.<domain>.dlq.v1"