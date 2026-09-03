CREATE TABLE maintenance_jobs (
    name             TEXT PRIMARY KEY,
    class            TEXT NOT NULL CHECK (class IN ('derived', 'ephemeral')),
    description      TEXT NOT NULL DEFAULT '',
    interval_seconds INTEGER NOT NULL CHECK (interval_seconds > 0),
    next_run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner      TEXT NOT NULL DEFAULT '',
    lease_until      TIMESTAMPTZ,
    last_run_id      BIGINT,
    last_status      TEXT NOT NULL DEFAULT 'never'
                     CHECK (last_status IN ('never', 'running', 'succeeded', 'failed')),
    last_started_at  TIMESTAMPTZ,
    last_completed_at TIMESTAMPTZ,
    last_success_at  TIMESTAMPTZ,
    last_report      JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_error       TEXT NOT NULL DEFAULT '',
    run_count        BIGINT NOT NULL DEFAULT 0,
    failure_count    BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_maintenance_jobs_due
    ON maintenance_jobs (next_run_at, name);

CREATE TABLE maintenance_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_name    TEXT NOT NULL,
    class       TEXT NOT NULL CHECK (class IN ('derived', 'ephemeral')),
    trigger     TEXT NOT NULL CHECK (trigger IN ('automatic', 'manual')),
    dry_run     BOOLEAN NOT NULL DEFAULT false,
    status      TEXT NOT NULL DEFAULT 'running'
                CHECK (status IN ('running', 'succeeded', 'failed')),
    lease_owner TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    report      JSONB NOT NULL DEFAULT '{}'::jsonb,
    error       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_maintenance_runs_recent
    ON maintenance_runs (started_at DESC, id DESC);

ALTER TABLE maintenance_jobs
    ADD CONSTRAINT maintenance_jobs_last_run_fk
    FOREIGN KEY (last_run_id) REFERENCES maintenance_runs(id) ON DELETE SET NULL;
