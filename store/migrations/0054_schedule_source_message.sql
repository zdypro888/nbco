-- Preserve the user turn that authored a schedule. Scheduled AI uses the
-- source message and its timestamp to resolve temporal language at delivery.
ALTER TABLE schedules
    ADD COLUMN source_message_id BIGINT REFERENCES chat_messages(id) ON DELETE SET NULL;
