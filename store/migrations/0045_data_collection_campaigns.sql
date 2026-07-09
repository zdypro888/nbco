CREATE TABLE IF NOT EXISTS data_collection_campaigns (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title           TEXT NOT NULL,
    instruction     TEXT NOT NULL DEFAULT '',
    required_fields TEXT[] NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'active',
    created_by      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_data_campaigns_status ON data_collection_campaigns(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_data_campaigns_created_by ON data_collection_campaigns(created_by, id DESC);

CREATE TABLE IF NOT EXISTS data_collection_campaign_targets (
    campaign_id      BIGINT NOT NULL REFERENCES data_collection_campaigns(id) ON DELETE CASCADE,
    user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status           TEXT NOT NULL DEFAULT 'pending',
    missing_fields   TEXT[] NOT NULL DEFAULT '{}',
    last_notified_at TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_data_campaign_targets_user ON data_collection_campaign_targets(user_id, status);
CREATE INDEX IF NOT EXISTS idx_data_campaign_targets_campaign_status ON data_collection_campaign_targets(campaign_id, status);
