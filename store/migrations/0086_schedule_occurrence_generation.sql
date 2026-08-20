-- A wall-clock instant is schedule data, not the identity of a delivery
-- occurrence. A schedule may legitimately be moved away from an instant and
-- later moved back; retain a monotonic generation so that occurrence is still
-- independently deliverable.

ALTER TABLE schedule_deliveries
    ADD COLUMN occurrence_generation BIGINT;

WITH ranked AS (
    SELECT id,
           dense_rank() OVER (PARTITION BY schedule_id ORDER BY occurrence_at) AS generation
      FROM schedule_deliveries
)
UPDATE schedule_deliveries d
   SET occurrence_generation = ranked.generation
  FROM ranked
 WHERE d.id = ranked.id;

ALTER TABLE schedule_deliveries
    ALTER COLUMN occurrence_generation SET NOT NULL;
ALTER TABLE schedule_deliveries
    ADD CONSTRAINT schedule_deliveries_occurrence_generation_positive
    CHECK (occurrence_generation > 0);

ALTER TABLE schedules
    ADD COLUMN occurrence_generation BIGINT NOT NULL DEFAULT 1
    CHECK (occurrence_generation > 0);

UPDATE schedules s
   SET occurrence_generation = COALESCE((
       SELECT max(d.occurrence_generation) + 1
         FROM schedule_deliveries d
        WHERE d.schedule_id = s.id
   ), 1);

ALTER TABLE schedule_deliveries
    DROP CONSTRAINT schedule_deliveries_schedule_id_occurrence_at_user_id_key;
ALTER TABLE schedule_deliveries
    ADD CONSTRAINT schedule_deliveries_occurrence_generation_user_key
    UNIQUE (schedule_id, occurrence_generation, user_id);

CREATE OR REPLACE FUNCTION advance_schedule_occurrence_generation()
RETURNS trigger AS $$
BEGIN
    IF ROW(NEW.user_id, NEW.kind, NEW.message, NEW.fire_at, NEW.interval_s,
           NEW.target, NEW.mode, NEW.daily_at, NEW.weekdays, NEW.title,
           NEW.recipient_policy)
       IS DISTINCT FROM
       ROW(OLD.user_id, OLD.kind, OLD.message, OLD.fire_at, OLD.interval_s,
           OLD.target, OLD.mode, OLD.daily_at, OLD.weekdays, OLD.title,
           OLD.recipient_policy) THEN
        NEW.occurrence_generation := OLD.occurrence_generation + 1;
        NEW.delivery_claimed_at := NULL;
    ELSE
        -- Generation is owned by this trigger, not by arbitrary update paths.
        NEW.occurrence_generation := OLD.occurrence_generation;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER schedules_advance_occurrence_generation
    BEFORE UPDATE ON schedules
    FOR EACH ROW EXECUTE FUNCTION advance_schedule_occurrence_generation();
