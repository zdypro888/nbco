-- Terminal receipts are retained for operational diagnosis, then removed by
-- the scheduler. Uncertain started rows are never selected by that cleanup.
CREATE INDEX idx_notification_deliveries_terminal_age
    ON notification_deliveries(updated_at)
    WHERE status IN ('delivered','failed');

CREATE INDEX idx_external_action_receipts_terminal_age
    ON external_action_receipts(updated_at)
    WHERE status IN ('completed','failed');
