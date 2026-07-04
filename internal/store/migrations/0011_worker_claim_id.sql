-- Worker claim generation token. A stale worker process must not be able to
-- submit progress/results after the same task has been re-claimed.
ALTER TABLE tasks ADD COLUMN worker_claim_id TEXT NOT NULL DEFAULT '';
