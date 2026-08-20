-- 0074 was deployed by some installations before its file-intake canonical
-- column was added to the migration body. Migration names are immutable once
-- recorded, so repair those installations with a new forward-only migration.
ALTER TABLE file_intakes ADD COLUMN IF NOT EXISTS canonical BOOLEAN;

ALTER TABLE file_intakes ALTER COLUMN canonical SET DEFAULT TRUE;

UPDATE file_intakes SET canonical = TRUE WHERE external_ref = '' OR external_ref IS NULL;
UPDATE file_intakes SET canonical = FALSE WHERE external_ref <> '';

WITH preferred AS (
    SELECT DISTINCT ON (user_id, source, external_ref) id
      FROM file_intakes
     WHERE external_ref <> ''
     ORDER BY user_id, source, external_ref,
              CASE status WHEN 'saved' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,
              id DESC
)
UPDATE file_intakes intake SET canonical = TRUE
  FROM preferred WHERE intake.id = preferred.id;

UPDATE file_intakes SET canonical = TRUE WHERE canonical IS NULL;
ALTER TABLE file_intakes ALTER COLUMN canonical SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_file_intakes_canonical_source
    ON file_intakes(user_id, source, external_ref)
    WHERE canonical AND external_ref <> '';
