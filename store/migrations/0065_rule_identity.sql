-- A policy title is its stable logical identity inside one scope. Saving the
-- same rule again updates that policy instead of appending another active row.
-- The trigger keeps the invariant valid for generic update/rollback paths too.
CREATE OR REPLACE FUNCTION nbco_rule_identity(p_kind TEXT, p_title TEXT, p_tags TEXT[])
RETURNS TEXT
LANGUAGE plpgsql
IMMUTABLE
PARALLEL SAFE
AS $$
DECLARE
    normalized_scope TEXT;
BEGIN
    IF p_kind <> 'policy' THEN
        RETURN '';
    END IF;

    SELECT lower(btrim(substr(tag, 7)))
      INTO normalized_scope
      FROM unnest(COALESCE(p_tags, '{}'::TEXT[])) AS tag
     WHERE tag LIKE 'scope:%'
     ORDER BY tag
     LIMIT 1;

    normalized_scope := COALESCE(NULLIF(normalized_scope, ''), 'global');
    RETURN md5(
        lower(regexp_replace(btrim(p_title), '\s+', ' ', 'g')) || chr(31) || normalized_scope
    );
END;
$$;

ALTER TABLE knowledge
    ADD COLUMN rule_identity TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION nbco_set_rule_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.rule_identity := nbco_rule_identity(NEW.kind, NEW.title, NEW.tags);
    RETURN NEW;
END;
$$;

CREATE TRIGGER knowledge_rule_identity_trigger
BEFORE INSERT OR UPDATE OF kind, title, tags ON knowledge
FOR EACH ROW EXECUTE FUNCTION nbco_set_rule_identity();

UPDATE knowledge
   SET rule_identity = nbco_rule_identity(kind, title, tags);

-- Preserve the discarded active state in the normal version ledger before
-- archiving duplicates. The newest row is canonical because it reflects the
-- latest explicit save/update.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY rule_identity ORDER BY id DESC) AS position
      FROM knowledge
     WHERE kind = 'policy' AND active AND rule_identity <> ''
), duplicates AS (
    SELECT k.*
      FROM knowledge k
      JOIN ranked r ON r.id = k.id
     WHERE r.position > 1
)
INSERT INTO knowledge_versions
    (knowledge_id, version, title, content, tags, kind, pinned, active, changed_by, change_note)
SELECT d.id,
       COALESCE((SELECT max(v.version) FROM knowledge_versions v WHERE v.knowledge_id = d.id), 0) + 1,
       d.title, d.content, d.tags, d.kind, d.pinned, d.active, NULL,
       'archived duplicate by migration 0065'
  FROM duplicates d
ON CONFLICT (knowledge_id, version) DO NOTHING;

WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY rule_identity ORDER BY id DESC) AS position
      FROM knowledge
     WHERE kind = 'policy' AND active AND rule_identity <> ''
)
UPDATE knowledge k
   SET active = FALSE,
       embedding = NULL,
       embed_model = '',
       updated_at = now()
  FROM ranked r
 WHERE r.id = k.id AND r.position > 1;

CREATE UNIQUE INDEX knowledge_active_rule_identity_uidx
    ON knowledge (rule_identity)
    WHERE kind = 'policy' AND active AND rule_identity <> '';
