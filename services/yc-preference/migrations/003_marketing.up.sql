-- Marketing / newsletter opt-in flags (docs/features/OAuth profile and marketing preferences.md).
ALTER TABLE preferences
    ADD COLUMN newsletter BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN marketing BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN new_products BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE preference_defaults
    ADD COLUMN newsletter BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN marketing BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN new_products BOOLEAN NOT NULL DEFAULT FALSE;
