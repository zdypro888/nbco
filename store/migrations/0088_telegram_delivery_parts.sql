-- Telegram limits text size and one logical response can cross the external
-- boundary several times. Track every part independently so a retry never
-- resends an acknowledged prefix or silently forgets an unsent suffix.

CREATE TABLE telegram_delivery_parts (
    delivery_key       TEXT NOT NULL,
    part_index         INTEGER NOT NULL CHECK (part_index >= 0),
    part_count         INTEGER NOT NULL CHECK (part_count > 0 AND part_index < part_count),
    chat_id            BIGINT NOT NULL,
    content_hash       TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'started'
                       CHECK (status IN ('started', 'delivered', 'failed')),
    telegram_message_id BIGINT,
    delivered_at       TIMESTAMPTZ,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (delivery_key, part_index)
);

CREATE INDEX telegram_delivery_parts_terminal_idx
    ON telegram_delivery_parts (updated_at)
    WHERE status IN ('delivered', 'failed');

CREATE TRIGGER telegram_delivery_parts_touch_updated_at
    BEFORE UPDATE ON telegram_delivery_parts
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();
