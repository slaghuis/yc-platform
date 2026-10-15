## Summary

<!-- One or two sentences on what this PR does. -->

## Checklist

- [ ] Linked to an issue or ADR
- [ ] Follows the standards in `docs/standards/`
- [ ] New or changed events have schemas and taxonomy entries
- [ ] New or changed APIs have OpenAPI spec updates
- [ ] Unit tests for new logic
- [ ] Integration tests for new DB or event interactions
- [ ] No PII in logs, traces, or event payloads where avoidable
- [ ] Tenant isolation respected (RLS, subject scoping)
- [ ] Backward compatible, or ADR raised for breaking change
- [ ] System admin capabilities (`/sysadmin/*` or `system_admin` routes, alert rules, runbooks, operator tools) added, changed or removed are noted for the release notes (ADR-028 rule 12)

## Risk

<!-- Operational, security, data risk. "None" is acceptable. -->
