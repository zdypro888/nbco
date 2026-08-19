package store

import (
	"context"
	"time"
)

// Grant 一条权限授予记录。
//
// Kind=active：User 能对 Target 做 Action（主动权限）。
// Kind=passive：Target 能对 User 做 Action（被动权限，挂在被作用者身上）。
// Target 是用户 ID 的十进制文本，或 "_all"。
type Grant struct {
	ID        int64
	Kind      string
	UserID    int64
	Action    string
	Target    string
	GrantedBy int64
	CreatedAt time.Time
}

// 权限维度。
const (
	KindActive  = "active"
	KindPassive = "passive"
)

// TargetAll 通配目标。
const TargetAll = "_all"

// Permission mutations are rare and graph-wide revocation pruning must not
// race a concurrent delegated grant into existence after cleanup.
const permissionGraphLock int64 = 77670021

const grantCols = `id, kind, user_id, action, target, granted_by, created_at`

// effectiveActiveGrantsCTE treats each active permission as a delegation edge.
// A root edge must have been issued by a currently active superadmin; every
// following edge must be issued by a holder of the same capability and may
// only narrow _all to an exact target. UNION (rather than UNION ALL) makes a
// malformed cycle finite while the schema remains inspectable and repairable.
const effectiveActiveGrantsCTE = `effective_active (` + grantCols + `) AS (
	SELECT p.id, p.kind, p.user_id, p.action, p.target, p.granted_by, p.created_at
	  FROM permissions p
	  JOIN users issuer ON issuer.id = p.granted_by
	  JOIN users holder ON holder.id = p.user_id
	 WHERE p.kind = 'active'
	   AND issuer.is_superadmin AND issuer.status = 'active'
	   AND holder.status = 'active'
	UNION
	SELECT child.id, child.kind, child.user_id, child.action, child.target, child.granted_by, child.created_at
	  FROM permissions child
	  JOIN effective_active parent
	    ON parent.user_id = child.granted_by
	   AND parent.action = child.action
	   AND (parent.target = '_all' OR parent.target = child.target)
	  JOIN users holder ON holder.id = child.user_id
	 WHERE child.kind = 'active' AND holder.status = 'active'
)`

// structuralActiveGrantsCTE ignores active/disabled status while preserving
// delegation structure. Disabled identities are suspended by the effective
// CTE, but an unrelated revocation must not silently turn that reversible
// suspension into permanent permission deletion. Explicit superadmin demotion
// still invalidates roots because is_superadmin is structural authority.
const structuralActiveGrantsCTE = `structural_active (` + grantCols + `) AS (
	SELECT p.id, p.kind, p.user_id, p.action, p.target, p.granted_by, p.created_at
	  FROM permissions p
	  JOIN users issuer ON issuer.id = p.granted_by
	  JOIN users holder ON holder.id = p.user_id
	 WHERE p.kind = 'active' AND issuer.is_superadmin
	UNION
	SELECT child.id, child.kind, child.user_id, child.action, child.target, child.granted_by, child.created_at
	  FROM permissions child
	  JOIN structural_active parent
	    ON parent.user_id = child.granted_by
	   AND parent.action = child.action
	   AND (parent.target = '_all' OR parent.target = child.target)
	  JOIN users holder ON holder.id = child.user_id
	 WHERE child.kind = 'active'
)`

// GrantPerm adds one authorization edge. The same capability may be conferred
// by independent grantors; repeating the same edge returns ErrConflict.
func (s *Store) GrantPerm(ctx context.Context, g Grant) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, permissionGraphLock); err != nil {
		return err
	}
	var allowed bool
	switch g.Kind {
	case KindActive:
		err = tx.QueryRow(ctx, `WITH RECURSIVE `+effectiveActiveGrantsCTE+`
			SELECT EXISTS (
				SELECT 1 FROM users issuer
				 WHERE issuer.id = $1 AND issuer.status = 'active' AND issuer.is_superadmin
			) OR (
				NOT EXISTS (SELECT 1 FROM users target_user WHERE target_user.id = $4 AND target_user.is_superadmin)
				AND EXISTS (
					SELECT 1 FROM effective_active management
					 WHERE management.user_id = $1 AND management.action = 'manage_perm'
					   AND (management.target = '_all' OR management.target = $4::text)
				)
				AND EXISTS (
					SELECT 1 FROM effective_active parent
					 WHERE parent.user_id = $1 AND parent.action = $2
					   AND (parent.target = '_all' OR parent.target = $3)
				)
			)`, g.GrantedBy, g.Action, g.Target, g.UserID).Scan(&allowed)
	case KindPassive:
		// A disclosure grant is direct consent from the subject, or a change
		// made by a live superadmin / holder of manage_perm over that subject.
		err = tx.QueryRow(ctx, `WITH RECURSIVE `+effectiveActiveGrantsCTE+`
			SELECT EXISTS (
				SELECT 1 FROM users self
				 WHERE self.id = $1 AND self.status = 'active' AND $1 = $2
			) OR EXISTS (
				SELECT 1 FROM users issuer
				 WHERE issuer.id = $1 AND issuer.status = 'active' AND issuer.is_superadmin
			) OR EXISTS (
				SELECT 1 FROM effective_active parent
				 WHERE parent.user_id = $1 AND parent.action = 'manage_perm'
				   AND (parent.target = '_all' OR parent.target = $2::text)
			)`, g.GrantedBy, g.UserID).Scan(&allowed)
	default:
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO permissions (kind, user_id, action, target, granted_by) VALUES ($1, $2, $3, $4, $5)`,
		g.Kind, g.UserID, g.Action, g.Target, g.GrantedBy); err != nil {
		return wrapErr(err)
	}
	return tx.Commit(ctx)
}

// RevokePerm removes the sources of one permission that actorID is allowed to
// control, then permanently prunes downstream active grants that no longer
// have a live authorization path. A superadmin may remove every source; the
// passive-permission subject may withdraw all consent; other managers may
// remove only their own grants or grants made by people they can manage. This
// prevents a peer or subordinate from erasing a higher authority's grant.
// Authorization is rechecked under the same graph lock as deletion, closing
// the tool-layer check/use race.
func (s *Store) RevokePerm(ctx context.Context, actorID int64, kind string, userID int64, action, target string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, permissionGraphLock); err != nil {
		return err
	}
	var allowed bool
	switch kind {
	case KindActive:
		err = tx.QueryRow(ctx, `WITH RECURSIVE `+effectiveActiveGrantsCTE+`
			SELECT EXISTS (
				SELECT 1 FROM users actor
				 WHERE actor.id = $1 AND actor.status = 'active' AND actor.is_superadmin
			) OR (
				NOT EXISTS (SELECT 1 FROM users target_user WHERE target_user.id = $2 AND target_user.is_superadmin)
				AND EXISTS (
					SELECT 1 FROM effective_active management
					 WHERE management.user_id = $1 AND management.action = 'manage_perm'
					   AND (management.target = '_all' OR management.target = $2::text)
				)
				AND EXISTS (
					SELECT 1 FROM effective_active capability
					 WHERE capability.user_id = $1 AND capability.action = $3
					   AND (capability.target = '_all' OR capability.target = $4)
				)
			)`, actorID, userID, action, target).Scan(&allowed)
	case KindPassive:
		err = tx.QueryRow(ctx, `WITH RECURSIVE `+effectiveActiveGrantsCTE+`
			SELECT EXISTS (
				SELECT 1 FROM users self
				 WHERE self.id = $1 AND self.status = 'active' AND $1 = $2
			) OR EXISTS (
				SELECT 1 FROM users actor
				 WHERE actor.id = $1 AND actor.status = 'active' AND actor.is_superadmin
			) OR (
				NOT EXISTS (SELECT 1 FROM users target_user WHERE target_user.id = $2 AND target_user.is_superadmin)
				AND EXISTS (
					SELECT 1 FROM effective_active management
					 WHERE management.user_id = $1 AND management.action = 'manage_perm'
					   AND (management.target = '_all' OR management.target = $2::text)
				)
			)`, actorID, userID).Scan(&allowed)
	default:
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	tag, err := tx.Exec(ctx, `WITH RECURSIVE `+effectiveActiveGrantsCTE+`
		DELETE FROM permissions p
		 WHERE p.kind = $2 AND p.user_id = $3 AND p.action = $4 AND p.target = $5
		   AND (
			EXISTS (
				SELECT 1 FROM users actor
				 WHERE actor.id = $1 AND actor.status = 'active' AND actor.is_superadmin
			)
			OR ($2 = 'passive' AND $1 = $3)
			OR p.granted_by = $1
			OR (
				NOT EXISTS (
					SELECT 1 FROM users grantor
					 WHERE grantor.id = p.granted_by AND grantor.is_superadmin
				)
				AND EXISTS (
					SELECT 1 FROM effective_active management
					 WHERE management.user_id = $1 AND management.action = 'manage_perm'
					   AND (management.target = '_all' OR management.target = p.granted_by::text)
				)
			)
		   )`, actorID, kind, userID, action, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM permissions
				 WHERE kind = $1 AND user_id = $2 AND action = $3 AND target = $4
			)`, kind, userID, action, target).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrForbidden
		}
		return ErrNotFound
	}
	if kind == KindActive {
		// Revocation is permanent: invalid descendant edges are deleted rather
		// than left dormant to reappear after a later unrelated grant.
		if _, err := tx.Exec(ctx, `WITH RECURSIVE `+structuralActiveGrantsCTE+`
			DELETE FROM permissions p
			 WHERE p.kind = 'active'
			   AND NOT EXISTS (SELECT 1 FROM structural_active e WHERE e.id = p.id)`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// PermsOf returns effective permissions only. Active rows whose issuer no
// longer has a live chain to an active superadmin root are not authorization.
// Passive rows are direct disclosure settings and are not transitive.
func (s *Store) PermsOf(ctx context.Context, userID int64) ([]Grant, error) {
	return s.queryGrants(ctx,
		`WITH RECURSIVE `+effectiveActiveGrantsCTE+`
		 SELECT `+grantCols+` FROM effective_active WHERE user_id = $1
		 UNION ALL
		 SELECT `+grantCols+` FROM permissions WHERE kind = 'passive' AND user_id = $1
		 ORDER BY id`, userID)
}

// PassivePermsToward 取「谁对 subject 有被动授权」之外，还需要 _all 通配：
// 返回挂在 subject 身上的全部被动授权记录。
func (s *Store) PassivePermsToward(ctx context.Context, subjectID int64) ([]Grant, error) {
	return s.queryGrants(ctx,
		`SELECT `+grantCols+`
		 FROM permissions WHERE kind = 'passive' AND user_id = $1 ORDER BY id`, subjectID)
}

func (s *Store) queryGrants(ctx context.Context, sql string, args ...any) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.Kind, &g.UserID, &g.Action, &g.Target, &g.GrantedBy, &g.CreatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}
