ALTER TABLE schedules ADD COLUMN IF NOT EXISTS source_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS source_key TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_active_source
    ON schedules (created_by, source_kind, source_key)
    WHERE status = 'active' AND source_kind <> '' AND source_key <> '';

CREATE INDEX IF NOT EXISTS idx_schedules_active_source_lookup
    ON schedules (source_kind, source_key)
    WHERE status = 'active' AND source_kind <> '' AND source_key <> '';
