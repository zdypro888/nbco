-- One logical notification may be observed by several workers or reclaimed
-- after a process crash. Cross the irreversible boundary before calling the
-- external channel so a later claimant never sends the same occurrence twice.
CREATE TABLE notification_deliveries (
    delivery_key TEXT PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'started'
                 CHECK (status IN ('started','delivered','failed')),
    attempts     INTEGER NOT NULL DEFAULT 1 CHECK (attempts > 0),
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_notification_deliveries_user_recent
    ON notification_deliveries(user_id, created_at DESC);

-- Stable domain occurrences can opt out of the short event dedupe window.
ALTER TABLE events ADD COLUMN source_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_events_stable_source
    ON events(decider_id, source_key) WHERE source_key <> '';

-- Worker transport requests carry their own identities. Progress retries must
-- not create two timeline rows and artifact retries must resolve to one file.
ALTER TABLE worker_run_progress ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_worker_run_progress_request
    ON worker_run_progress(run_id, request_id) WHERE request_id <> '';

ALTER TABLE worker_run_files ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_worker_run_files_request
    ON worker_run_files(run_id, request_id) WHERE request_id <> '';
