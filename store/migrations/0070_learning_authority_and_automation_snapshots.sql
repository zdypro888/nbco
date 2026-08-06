-- Periodic maintenance needs an immutable input set. The calendar window may
-- stay open for retries, but newly-created rows must wait for the next cycle.
CREATE TABLE automation_snapshots (
    automation_key TEXT NOT NULL,
    occurrence_key TEXT NOT NULL,
    subject_id     BIGINT NOT NULL DEFAULT 0,
    item_kind      TEXT NOT NULL,
    item_ids       BIGINT[] NOT NULL DEFAULT '{}',
    expires_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (automation_key, occurrence_key, subject_id)
);

CREATE INDEX idx_automation_snapshots_expiry
    ON automation_snapshots (expires_at)
    WHERE expires_at IS NOT NULL;

-- A learning candidate's kind describes its shape. memory_class describes
-- which system owns the truth and therefore whether it may become durable
-- semantic memory.
ALTER TABLE learning_candidates
    ADD COLUMN memory_class TEXT NOT NULL DEFAULT 'unclassified'
    CHECK (memory_class IN ('unclassified', 'durable', 'canonical', 'transient'));

UPDATE learning_candidates
   SET memory_class = CASE kind
       WHEN 'rule' THEN 'durable'
       WHEN 'skill' THEN 'durable'
       WHEN 'script' THEN 'durable'
       WHEN 'profile' THEN 'canonical'
       WHEN 'summary' THEN 'transient'
       ELSE memory_class
   END;

CREATE INDEX idx_learning_candidates_governance
    ON learning_candidates (status, memory_class, id DESC);

-- The canonical chat transcript may include trusted execution context (file
-- IDs, speaker labels, host metadata). Learning must preserve the actual user
-- evidence separately so synthetic context cannot become a rule or skill.
ALTER TABLE memory_mining_jobs
    ADD COLUMN user_evidence_text TEXT;
