ALTER TABLE learning_candidates
    DROP CONSTRAINT IF EXISTS learning_candidates_published_knowledge_id_fkey;

ALTER TABLE learning_candidates
    ADD CONSTRAINT learning_candidates_published_knowledge_id_fkey
    FOREIGN KEY (published_knowledge_id) REFERENCES knowledge(id) ON DELETE SET NULL;
