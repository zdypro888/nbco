-- An event turn is a notification decision, never an authorization boundary for
-- mutating business state. Critical domain events must survive an AI "skip"
-- decision and are delivered with a factual fallback when necessary.
ALTER TABLE events
    ADD COLUMN notification_required BOOLEAN NOT NULL DEFAULT FALSE;
