-- Employee invites can carry a role/title such as "CEO"; it is copied into
-- users.info.role when the invite is redeemed.
ALTER TABLE bind_keys ADD COLUMN IF NOT EXISTS invited_role TEXT NOT NULL DEFAULT '';

INSERT INTO info_fields (name) VALUES ('role') ON CONFLICT DO NOTHING;
