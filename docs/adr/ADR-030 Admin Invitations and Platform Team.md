# ADR-030: Admin Invitations and Platform Team

**Status:** Proposed
**Date:** 2026-10-15
**Context:** v1 has two ways to create an administrator, and neither works from a console. The first system admin comes from `yc-identity bootstrap-admin` on the server; there is no way to add a second. Shop admins register as customers on their shop and a system admin promotes them (`POST /admin/users/{id}/promote`, `role: shop_admin` only), which needs the two people to coordinate and is the first item in backlog-v2 P1. `platform.tenant.provisioned.v1` already carries `primary_admin_email` ("invited by yc-message"), but nothing acts on it. Demote and suspend are in yc-identity's spec but were never built. The console (yc-platform-admin) needs to manage the platform team, and creating a shop there should hand it to its first shop admin without a manual step. Versioned event schemas are immutable, so `identity.user.registered.v1` (role `customer` only) and `identity.user.promoted.v1` (to `shop_admin` at most) cannot describe an invited administrator.

## Decision

**yc-identity gains invitations: a system admin invites an email to a role in a tenant, yc-message sends a single-use link to that tenant's host, and accepting it sets a password and activates the user. Invitations are the only way to create system admins after the first, and the standard way to create shop admins. yc-identity also implements suspend, reactivate and demote for administrators.**

## Rules

### Invitations

1. **Who invites whom.** System admins invite `system_admin` (tenant must be `_platform`) and `shop_admin` (tenant must be an active shop). Shop admins cannot invite in v1.1.
2. **Endpoints** (system admin, platform host):

   | Method | Path | Effect |
   |---|---|---|
   | POST | `/sysadmin/invitations` | `{tenant_id, email, role, display_name?}`; `Idempotency-Key` required |
   | GET | `/sysadmin/invitations` | Filter by `tenant_id`, `status` (`pending`, `accepted`, `revoked`, `expired`) |
   | POST | `/sysadmin/invitations/{id}/resend` | New token, old one void |
   | DELETE | `/sysadmin/invitations/{id}` | Revoke a pending invitation |

   And one anonymous endpoint, reachable on shop and platform hosts: `POST /invitations/accept {token, password, display_name?}`.
3. **Token.** 32 random bytes, stored by SHA-256 hash like reset tokens, single use, valid for `invitations.ttl` (72 h). Only the latest token of an invitation is valid.
4. **Link.** yc-identity calls yc-message `POST /internal/send` (ADR-023 rule 6) with type `admin_invite` and a link to `https://<host>/account/accept-invite?token=…`. `<host>` is the tenant's primary hostname (ADR-029), or the first platform host for `_platform`. The storefront and the console each implement that page (ADR-027 rules 12 and 13).
5. **Existing users.** An email that is already a customer of the shop gets an invitation like anyone else; accepting it upgrades that account in place (keeping its ID, orders and cart) and asks for the current password instead of a new one. An email that is already an admin of that tenant is a `409`.
6. **Accepting** sets the password (password policy as for registration), sets `status = active` and the invited role, and publishes `identity.user.invitation_accepted.v1`. Tokens of an upgraded customer stop validating, as with promotion.
7. **First shop admin.** yc-identity consumes `platform.tenant.provisioned.v1` and, when `primary_admin_email` is set, creates the invitation itself (actor: the system admin in the provisioning event's envelope). Creating a shop in the console therefore sends its owner an invite without a second call.

### Platform team and administrators

8. **Suspend and reactivate** (`POST /admin/users/{id}/suspend`, `/reactivate`, system admin): a suspended user cannot log in, its refresh families are revoked and its access tokens stop validating at the next check (gateway cache TTL, 60 s). A system admin cannot suspend themselves, and the last active system admin cannot be suspended.
9. **Demote** (`POST /admin/users/{id}/demote`, system admin): `shop_admin` → `customer` in the same tenant. System admins are not demoted; they are suspended.
10. **No deletion of system admins** (unchanged from v1: `DELETE /admin/users/{id}` on a system admin is `409`). Their audit trail must survive; suspension removes access.
11. **`promote` stays** for upgrading a known customer without email, but the console uses invitations.

### Events

12. **New schemas** (identity domain, ADR-007):
    - `identity.user.invited.v1`: invitation, tenant, role, email (PII), expiry. Published on create and on resend.
    - `identity.user.invitation_accepted.v1`: user, tenant, role, whether an existing customer was upgraded.
    - `identity.user.invitation_revoked.v1`.
    - `identity.user.suspended.v1` and `identity.user.reactivated.v1` (the first is listed in yc-identity.md but has no schema yet).
    - `identity.user.demoted.v1` (listed, no schema yet).
13. **Consumers.** yc-message adds `invitation_accepted` and `demoted` to its admin projection, which feeds alert audiences (ADR-025 rule 11): accepted shop admins join their shop's audience, accepted system admins join the system audience next to the configured `admins.system_emails`, and suspended users leave both. It sends `admin_invite` from a new platform-default template. yc-audit records all six. yc-preference creates defaults for a new user on `invitation_accepted` as it does on `registered`.

## Consequences

**Positive**
- The console can run the platform team and hand over new shops without anyone touching a server or coordinating a register-then-promote.
- `primary_admin_email` finally does what its schema says.
- The same accept page pattern works on every host, so shops on their own domains need nothing special.

**Negative**
- New public surface: an anonymous accept endpoint. It is rate-limited like `/login`, returns the same error for unknown, used and expired tokens, and counts failures like login (ADR-026 rule 11).
- Six new event types and changes in yc-message, yc-audit and yc-preference for what looks like one feature.
- An invited system admin's email must be reachable for the link; there is no out-of-band path except `bootstrap-admin`.

## Alternatives Considered

- **Create admins with a temporary password shown to the inviter**: rejected; the password travels outside the platform and the inviter knows it.
- **Reuse the password-reset flow** (create the user, then send a reset): rejected; the email would say "reset", expiry and audit would be wrong, and a pending invite could not be listed or revoked.
- **Extend `identity.user.promoted.v1` to allow `system_admin`**: rejected; versioned schemas are immutable (event-conventions), and an invitation is not a promotion of an existing user.
- **Let shop admins invite other shop admins now**: deferred; it needs per-shop limits and a decision on who may remove whom.
