-- Human employee invites are still backed by one-time bind keys, but carry
-- invite metadata so they are no longer anonymous "generic tokens".
ALTER TABLE bind_keys ADD COLUMN IF NOT EXISTS invited_name TEXT NOT NULL DEFAULT '';
ALTER TABLE bind_keys ADD COLUMN IF NOT EXISTS note TEXT NOT NULL DEFAULT '';
