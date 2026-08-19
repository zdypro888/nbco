-- A permission row is an authorization edge from granted_by to user_id.
-- Multiple independent grantors may confer the same capability; effective
-- authorization is resolved from live superadmin roots at read time.
ALTER TABLE permissions
    DROP CONSTRAINT IF EXISTS permissions_kind_user_id_action_target_key;

-- These capabilities apply to a system/owned-resource domain rather than a
-- person. Older rows accepted arbitrary user targets even though handlers
-- treated every such row as global, so normalize them before tightening the
-- uniqueness rule.
WITH duplicate_global AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY kind, user_id, action, granted_by
               ORDER BY id
           ) AS position
      FROM permissions
     WHERE kind = 'active'
       AND action IN ('generate_key', 'manage_worker')
)
DELETE FROM permissions p
 USING duplicate_global d
 WHERE p.id = d.id AND d.position > 1;

UPDATE permissions
   SET target = '_all'
 WHERE kind = 'active'
   AND action IN ('generate_key', 'manage_worker')
   AND target <> '_all';

-- Old schemas did not constrain the issuer. Remove impossible orphan edges
-- before adding the referential contract.
DELETE FROM permissions p
 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = p.granted_by);

ALTER TABLE permissions
    ADD CONSTRAINT permissions_kind_check
        CHECK (kind IN ('active', 'passive')),
    ADD CONSTRAINT permissions_granted_by_fkey
        FOREIGN KEY (granted_by) REFERENCES users(id) ON DELETE CASCADE,
    ADD CONSTRAINT permissions_edge_key
        UNIQUE (kind, user_id, action, target, granted_by);

CREATE INDEX permissions_grantor_action_idx
    ON permissions (granted_by, kind, action, target);
