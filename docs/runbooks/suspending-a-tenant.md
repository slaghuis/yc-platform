# Suspending, Reinstating and Deleting a Tenant

Take a shop offline, bring it back, or free its hostnames after a suspension. System admins only, on platform hosts (ADR-029).

## Prerequisites

- System admin access to the platform host (`Authorization: Bearer …`).
- The tenant ID (e.g. `shop-abc`). Confirm with `GET /sysadmin/tenants/{id}` or `GET /sysadmin/tenants?q=…`.

## Suspend

Moves `active` → `suspended`. yc-gateway answers `503 tenant_suspended` on every route for the shop's hosts, including webhooks. Data is untouched; in-flight sagas finish or expire on their own.

```bash
curl -X POST "$GATEWAY/sysadmin/tenants/shop-abc/suspend" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"reason":"Non-payment; contact ops@example"}'
```

`reason` is required (1–500 characters). Suspending a non-active tenant is `409`.

## Reinstate

Moves `suspended` → `active`. Traffic resumes on the shop's hosts.

```bash
curl -X POST "$GATEWAY/sysadmin/tenants/shop-abc/reinstate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)"
```

## Delete

Moves `suspended` → `deleted`. The tenant must already be suspended. yc-gateway answers `410` on the former hosts and frees the hostnames so another shop can claim them. The `tenant_id` is never reused. Per-service data purge is backlog-v2; `data_purged_at` stays null until then.

```bash
curl -X DELETE "$GATEWAY/sysadmin/tenants/shop-abc" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"confirm_tenant_id":"shop-abc"}'
```

`confirm_tenant_id` must repeat the path ID. Deleting an active tenant is `409` — suspend first.

## Audit

Every transition publishes a lifecycle event on `_platform` with the acting system admin as the envelope actor. `GET /sysadmin/audit?category=tenant_lifecycle` on yc-audit lists them.

## Related

- `onboarding-tenant.md`
- `docs/services/yc-tenant.md`, ADR-029
