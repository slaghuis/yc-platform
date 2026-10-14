# ADR-003: Dead Letter Topics

**Status:** Accepted
**Date:** 2025-01-15

## Decision

Every consumer routes unprocessable messages ("poison messages") to a tenant-scoped DLQ subject:

```
yc.<tenant_id>.<domain>.dlq.v1
```

A message is considered poison when:
- It fails schema validation
- It fails processing after N retries (default: 5, with exponential backoff: 1s, 5s, 30s, 2m, 10m)
- It throws a non-retriable error (explicit `ErrPoison` from the handler)
- Its envelope `tenant_id` differs from the subject's tenant segment (ADR-001). These, and envelopes that cannot be parsed, go to the `_platform` DLQ rather than a tenant's, so a replay can never deliver them to the wrong shop.

*Amended 2026-10-14: tenant-mismatch criterion and `_platform` DLQ routing added; `service` added to `dlq_metadata` alongside the durable `consumer` name.*

## Rules

1. **The envelope is preserved** — the DLQ message is the original event wrapped with `dlq_metadata`:
   ```json
   {
     "original_event": { ...full envelope... },
     "dlq_metadata": {
       "service": "yc-orders",
       "consumer": "yc-orders-payment-captured",
       "attempts": 5,
       "first_failure": "2025-01-15T10:00:00Z",
       "last_error": "unmarshal: unknown field 'foo'",
       "last_failure": "2025-01-15T10:12:00Z"
     }
   }
   ```
2. **YC-Monitor subscribes** to `yc.*.*.dlq.v1` and raises alerts
3. **DLQ is per-tenant** — one tenant's bad messages don't pollute another's visibility
4. **Manual replay tooling** must exist from day one (CLI in `yc-eventbus` library)
5. **DLQ retention: 90 days** — long enough for human investigation, short enough to bound storage
