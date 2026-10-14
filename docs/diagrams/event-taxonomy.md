# Event Taxonomy

Registry of every event in yc-platform. One source of truth; CI verifies
that every schema file has a catalog entry and every entry has a schema file.

## Columns

| Event Type | Producer | Primary Consumers | Trigger |
|---|---|---|---|

## Platform Domain

| Event Type | Producer | Primary Consumers | Trigger |
|---|---|---|---|
| `platform.tenant.provisioned.v1`         | yc-tenant-cli | All services | Admin provisions tenant |
| `platform.tenant.suspended.v1`           | yc-tenant-cli | yc-gateway, yc-orders | Admin suspends tenant |
| `platform.tenant.deleted.v1`             | yc-tenant-cli | All services | Admin deletes tenant |
| `platform.tenant.configuration_changed.v1` | yc-tenant-cli | Interested services | Admin updates tenant config |
| `platform.service.registered.v1`         | yc-gateway   | yc-monitor | Service self-registers |
| `platform.service.deregistered.v1`       | yc-gateway   | yc-monitor | Service deregisters |
| `platform.gdpr.cascade_completed.v1`     | Any service  | yc-audit | Service completes GDPR deletion |

## Identity Domain

| Event Type | Producer | Primary Consumers | Trigger |
|---|---|---|---|
| `identity.visitor.created.v1`            | yc-identity  | yc-monitor, yc-analytics | Visitor token issued |
| `identity.user.registered.v1`            | yc-identity  | yc-preference, yc-message | Customer registers |
| `identity.user.logged_in.v1`             | yc-identity  | yc-cart (merge), yc-analytics | Successful login |
| `identity.user.login_failed.v1`          | yc-identity  | yc-audit (threshold) | Failed login |
| `identity.user.password_reset_requested.v1` | yc-identity | yc-message | Reset flow started |
| `identity.user.password_changed.v1`      | yc-identity  | yc-audit | Password changed |
| `identity.user.promoted.v1`              | yc-identity  | yc-gateway (cache), yc-audit | Role elevated |
| `identity.user.demoted.v1`               | yc-identity  | yc-gateway (cache), yc-audit | Role reduced |
| `identity.user.suspended.v1`             | yc-identity  | yc-audit | User suspended |
| `identity.user.deleted.v1`               | yc-identity  | All PII-holding services | GDPR cascade trigger |
| `identity.oauth_linked.v1`               | yc-identity  | yc-audit | OAuth provider linked |
| `identity.oauth_unlinked.v1`             | yc-identity  | yc-audit | OAuth provider unlinked |

<!-- Fill in remaining domains from the event taxonomy session:
     product, pricing, inventory, cart, preference, review,
     checkout, payment, orders, shipping, message, image, audit, monitor -->