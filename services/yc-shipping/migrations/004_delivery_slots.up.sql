-- Delivery schedules and slot bookings (issue #30, ADR-022 amendment).
-- Weekdays: 0 = Sunday … 6 = Saturday (Go time.Weekday).

CREATE TABLE delivery_schedules (
    id                   UUID PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    name                 TEXT NOT NULL,
    timezone             TEXT NOT NULL DEFAULT 'Africa/Johannesburg',
    window_minutes       INT NOT NULL CHECK (window_minutes BETWEEN 5 AND 240),
    weekdays             INT[] NOT NULL,
    open_time            TIME NOT NULL,
    close_time           TIME NOT NULL,
    capacity_per_window  INT NOT NULL CHECK (capacity_per_window >= 1),
    min_lead_minutes     INT NOT NULL DEFAULT 0 CHECK (min_lead_minutes >= 0),
    same_day_cutoff      TIME,
    blackout_dates       DATE[] NOT NULL DEFAULT '{}',
    active               BOOLEAN NOT NULL DEFAULT TRUE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (open_time < close_time)
);
CREATE INDEX idx_delivery_schedules_tenant ON delivery_schedules (tenant_id, name, id);

CREATE TABLE slot_bookings (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    schedule_id UUID NOT NULL REFERENCES delivery_schedules (id) ON DELETE CASCADE,
    slot_start  TIMESTAMPTZ NOT NULL,
    slot_end    TIMESTAMPTZ NOT NULL,
    booking_key TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, schedule_id, slot_start, booking_key)
);
CREATE INDEX idx_slot_bookings_capacity ON slot_bookings (schedule_id, slot_start);
CREATE INDEX idx_slot_bookings_key ON slot_bookings (tenant_id, booking_key);

ALTER TABLE rates
    ADD COLUMN schedule_id UUID REFERENCES delivery_schedules (id) ON DELETE RESTRICT,
    ADD COLUMN requires_delivery_slot BOOLEAN NOT NULL DEFAULT FALSE;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['delivery_schedules', 'slot_bookings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
