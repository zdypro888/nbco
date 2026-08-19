-- A sender's authority to create a schedule and a recipient's ability to mute
-- it are separate concerns. Existing schedules remain optional; mandatory is
-- an explicit, permission-checked choice made by the tool layer.
ALTER TABLE schedules
    ADD COLUMN recipient_policy TEXT NOT NULL DEFAULT 'optional';

ALTER TABLE schedules
    ADD CONSTRAINT schedules_recipient_policy_check
    CHECK (recipient_policy IN ('optional', 'mandatory'));

