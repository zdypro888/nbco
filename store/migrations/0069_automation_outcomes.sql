-- Separate an automation's business outcome from report delivery. A generated
-- notification is not proof that the scheduled objective succeeded.

ALTER TABLE automation_runs
    ADD COLUMN outcome TEXT NOT NULL DEFAULT ''
    CHECK (outcome IN ('', 'succeeded', 'no_change', 'failed', 'uncertain'));

ALTER TABLE automation_runs
    ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_automation_runs_expiry
    ON automation_runs (expires_at)
    WHERE status IN ('pending', 'processing');

-- Legacy runs had no execution window and could remain pending forever after
-- their caller's calendar window closed. They remain reclaimable when the
-- current period is re-enqueued, because failed runs below the attempt ceiling
-- may be claimed again with a fresh expires_at.
UPDATE automation_runs
   SET status = 'failed', claimed_at = NULL, completed_at = now(),
       last_error = CASE WHEN last_error = '' THEN 'legacy automation window expired' ELSE last_error END,
       updated_at = now()
 WHERE status IN ('pending', 'processing')
   AND updated_at < now() - interval '48 hours';
