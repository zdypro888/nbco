-- Sequential semantic checks improve Agent feedback, but only a database
-- identity can make concurrent memory-miner and tool writes idempotent.
ALTER TABLE learning_candidates
    ADD COLUMN content_identity TEXT NOT NULL DEFAULT '';

-- Existing rows may contain historical duplicates. Give them collision-free
-- legacy identities; runtime exact-match checks continue to suppress new copies.
UPDATE learning_candidates
   SET content_identity = 'legacy:' || id::text
 WHERE content_identity = '';

CREATE UNIQUE INDEX learning_candidates_active_identity
    ON learning_candidates (kind, memory_class, scope, content_identity)
 WHERE status IN ('pending', 'published');
