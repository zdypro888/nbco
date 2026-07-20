-- A claimed run is not considered delivered until the worker sends its first
-- heartbeat. Existing attempts keep their timestamp and remain acknowledged;
-- new attempts start NULL and can be reclaimed quickly if the HTTP response is
-- lost before the worker receives it.
ALTER TABLE worker_run_attempts
    ALTER COLUMN heartbeat_at DROP NOT NULL,
    ALTER COLUMN heartbeat_at DROP DEFAULT;
