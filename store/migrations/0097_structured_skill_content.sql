-- Skill fields used to be encoded in Chinese presentation lines and parsed by
-- prefix matching. Convert that representation once, then enforce a versioned
-- JSON object at every durable boundary.
CREATE OR REPLACE FUNCTION nbco_valid_skill_content(value TEXT)
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
AS $$
DECLARE
    doc JSONB;
    key_count INTEGER;
BEGIN
    doc := value::jsonb;
    IF jsonb_typeof(doc) <> 'object' THEN
        RETURN FALSE;
    END IF;
    SELECT count(*) INTO key_count FROM jsonb_object_keys(doc);
    RETURN COALESCE(
        key_count = 5
        AND jsonb_typeof(doc->'version') = 'number'
        AND doc->>'version' = '1'
        AND jsonb_typeof(doc->'trigger') = 'string'
        AND length(btrim(doc->>'trigger')) > 0
        AND jsonb_typeof(doc->'summary') = 'string'
        AND length(btrim(doc->>'summary')) > 0
        AND jsonb_typeof(doc->'procedure') = 'string'
        AND length(btrim(doc->>'procedure')) > 0
        AND jsonb_typeof(doc->'constraints') = 'string',
        FALSE
    );
EXCEPTION WHEN OTHERS THEN
    RETURN FALSE;
END;
$$;

CREATE FUNCTION nbco_migrate_skill_content(skill_title TEXT, value TEXT)
RETURNS TEXT
LANGUAGE plpgsql
IMMUTABLE
AS $$
DECLARE
    trigger_text TEXT := '';
    summary_text TEXT := '';
    procedure_text TEXT := '';
    constraints_text TEXT := '';
    tail TEXT := '';
    marker_pos INTEGER := 0;
BEGIN
    IF nbco_valid_skill_content(value) THEN
        RETURN (value::jsonb)::text;
    END IF;

    marker_pos := strpos(value, '触发条件：');
    IF marker_pos > 0 THEN
        tail := substr(value, marker_pos + char_length('触发条件：'));
        trigger_text := btrim(split_part(tail, E'\n', 1));
    END IF;

    marker_pos := strpos(value, '摘要：');
    IF marker_pos > 0 THEN
        tail := substr(value, marker_pos + char_length('摘要：'));
        summary_text := btrim(split_part(tail, E'\n', 1));
    END IF;

    marker_pos := strpos(value, '执行方法：');
    IF marker_pos > 0 THEN
        tail := ltrim(substr(value, marker_pos + char_length('执行方法：')), E' \t\r\n');
        marker_pos := strpos(tail, E'\n限制与禁忌：');
        IF marker_pos > 0 THEN
            procedure_text := btrim(substr(tail, 1, marker_pos - 1));
            constraints_text := btrim(substr(tail, marker_pos + char_length(E'\n限制与禁忌：')));
        ELSE
            procedure_text := btrim(tail);
        END IF;
    END IF;

    IF trigger_text = '' THEN
        trigger_text := COALESCE(NULLIF(btrim(skill_title), ''), '执行该可复用流程时');
    END IF;
    IF summary_text = '' THEN
        summary_text := COALESCE(NULLIF(btrim(skill_title), ''), '可复用执行流程');
    END IF;
    IF procedure_text = '' THEN
        procedure_text := COALESCE(NULLIF(btrim(value), ''), summary_text);
    END IF;

    RETURN jsonb_build_object(
        'version', 1,
        'trigger', trigger_text,
        'summary', summary_text,
        'procedure', procedure_text,
        'constraints', constraints_text
    )::text;
END;
$$;

UPDATE knowledge
   SET content = nbco_migrate_skill_content(title, content),
       embedding = NULL,
       embed_model = '',
       updated_at = now()
 WHERE kind = 'skill';

UPDATE knowledge_versions
   SET content = nbco_migrate_skill_content(title, content)
 WHERE kind = 'skill';

UPDATE learning_candidates
   SET content = nbco_migrate_skill_content(title, content),
       updated_at = now()
 WHERE kind = 'skill';

DROP FUNCTION nbco_migrate_skill_content(TEXT, TEXT);

ALTER TABLE knowledge
    ADD CONSTRAINT knowledge_skill_content_valid
    CHECK (kind <> 'skill' OR nbco_valid_skill_content(content));

ALTER TABLE knowledge_versions
    ADD CONSTRAINT knowledge_versions_skill_content_valid
    CHECK (kind <> 'skill' OR nbco_valid_skill_content(content));

ALTER TABLE learning_candidates
    ADD CONSTRAINT learning_candidates_skill_content_valid
    CHECK (kind <> 'skill' OR nbco_valid_skill_content(content));
