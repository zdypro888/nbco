-- ihtml is mounted as nbco's per-user dynamic workspace. PostgreSQL remains
-- the authoritative store; the library owns the row format while nbco owns
-- schema rollout through the same ordered migration ledger as the rest of the
-- application.
CREATE TABLE IF NOT EXISTS ihtml_items (
    usr        TEXT NOT NULL,
    id         TEXT NOT NULL,
    typ        TEXT NOT NULL,
    title      TEXT NOT NULL,
    page       TEXT NOT NULL,
    ord        INTEGER NOT NULL,
    content    TEXT NOT NULL,
    meta       TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (usr, id)
);

CREATE TABLE IF NOT EXISTS ihtml_kv (
    usr TEXT NOT NULL,
    k   TEXT NOT NULL,
    v   TEXT NOT NULL,
    PRIMARY KEY (usr, k)
);

CREATE TABLE IF NOT EXISTS ihtml_revisions (
    usr         TEXT NOT NULL,
    id          TEXT NOT NULL,
    note        TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    item_count  INTEGER NOT NULL,
    items       TEXT NOT NULL,
    PRIMARY KEY (usr, id)
);

-- Every write advances this row first. It is both the optimistic concurrency
-- token and the cross-instance serialization point for one user's workspace.
CREATE TABLE IF NOT EXISTS ihtml_user_version (
    usr     TEXT PRIMARY KEY,
    version BIGINT NOT NULL
);
