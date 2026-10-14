# yc-preference

`yc-preference` maintains per-customer preferences: display currency, preferred messaging channel, preferred search facets, and the last-used facet state.

See `docs/services/yc-preference.md` for the full requirement spec.

## Local development

```bash
task run SERVICE=yc-preference
```

Database: `yc_preference` (`YC_PREFERENCE_DB_URL`).
