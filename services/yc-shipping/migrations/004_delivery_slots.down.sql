ALTER TABLE rates DROP COLUMN IF EXISTS requires_delivery_slot;
ALTER TABLE rates DROP COLUMN IF EXISTS schedule_id;
DROP TABLE IF EXISTS slot_bookings;
DROP TABLE IF EXISTS delivery_schedules;
