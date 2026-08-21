-- Machine-readable worker output is part of the execution contract, not a
-- marker embedded in a human summary. Keep both the declared JSON Schema and
-- the submitted value on the durable run/attempt for validation, replay and
-- audit. Material entity source identity makes post-commit retry idempotent.
ALTER TABLE worker_runs
    ADD COLUMN result_required BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN result_schema JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN result_handler TEXT NOT NULL DEFAULT '',
    ADD COLUMN structured_result JSONB;

ALTER TABLE worker_runs
    ADD CONSTRAINT worker_runs_result_schema_object
    CHECK (jsonb_typeof(result_schema) = 'object'),
    ADD CONSTRAINT worker_runs_structured_result_object
    CHECK (structured_result IS NULL OR jsonb_typeof(structured_result) = 'object'),
    ADD CONSTRAINT worker_runs_result_handler_contract
    CHECK (result_handler = '' OR result_required);

ALTER TABLE worker_run_attempts
    ADD COLUMN structured_result JSONB,
    ADD CONSTRAINT worker_run_attempts_structured_result_object
    CHECK (structured_result IS NULL OR jsonb_typeof(structured_result) = 'object');

ALTER TABLE material_entities
    ADD COLUMN source_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_item_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX material_entities_source_identity
    ON material_entities (source_type, source_ref, source_item_key)
    WHERE source_type <> '' AND source_ref <> '' AND source_item_key <> '';
