CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_tool ON audit_log(tool, id DESC);

UPDATE learning_candidates
   SET status = 'rejected',
       review_note = CASE
           WHEN review_note = '' THEN 'Rejected automatically: legacy natural-language action guard was retired.'
           ELSE review_note
       END,
       reviewed_at = COALESCE(reviewed_at, now()),
       updated_at = now()
 WHERE source_type = 'action_guard'
   AND status = 'pending';
