-- Recipients need an independent control plane for schedules created by other
-- users. schedule_id=0 is the user's default for every schedule; a concrete
-- schedule row overrides that default. Delivery history remains auditable even
-- when a notification is intentionally suppressed.
CREATE TABLE schedule_delivery_preferences (
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    schedule_id  BIGINT NOT NULL DEFAULT 0 CHECK (schedule_id >= 0),
    enabled      BOOLEAN NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, schedule_id)
);

CREATE TRIGGER schedule_delivery_preferences_touch_updated_at
    BEFORE UPDATE ON schedule_delivery_preferences
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

ALTER TABLE schedule_deliveries
    DROP CONSTRAINT IF EXISTS schedule_deliveries_status_check;

ALTER TABLE schedule_deliveries
    ADD CONSTRAINT schedule_deliveries_status_check
    CHECK (status IN ('pending', 'processing', 'delivered', 'suppressed', 'failed', 'cancelled'));
