package store

import (
	"context"
	"time"
)

// LifecycleResult reports only row counts keyed by stable dataset names.
// Ambiguous business rows never enter these maintenance plans.
type LifecycleResult struct {
	Inspected int64
	Reclaimed int64
	Details   map[string]int64
}

func (s *Store) MaintainCredentials(ctx context.Context, now time.Time, dryRun bool) (LifecycleResult, error) {
	queries := []lifecycleQuery{
		{name: "pending_approvals", count: `SELECT count(*) FROM pending_approvals WHERE expires_at <= $1`, apply: `DELETE FROM pending_approvals WHERE expires_at <= $1`},
		{name: "api_token_rotations", count: `SELECT count(*) FROM api_token_rotations WHERE expires_at <= $1`, apply: `DELETE FROM api_token_rotations WHERE expires_at <= $1`},
		{name: "external_action_results", count: `SELECT count(*) FROM external_action_receipts WHERE result_expires_at IS NOT NULL AND result_expires_at <= $1`, apply: `UPDATE external_action_receipts SET result_text='', result_expires_at=NULL WHERE result_expires_at IS NOT NULL AND result_expires_at <= $1`},
		{name: "worker_bind_codes", count: `SELECT count(*) FROM worker_bind_codes WHERE expires_at <= $1`, apply: `DELETE FROM worker_bind_codes WHERE expires_at <= $1`},
	}
	return s.runLifecycleQueries(ctx, dryRun, queries, now)
}

func (s *Store) MaintainRetainedOperationalData(
	ctx context.Context,
	now time.Time,
	receiptRetention, telegramRetention, runtimeRetention, snapshotRetention, runRetention time.Duration,
	dryRun bool,
) (LifecycleResult, error) {
	receiptBefore := now.Add(-receiptRetention)
	telegramBefore := now.Add(-telegramRetention)
	runtimeBefore := now.Add(-runtimeRetention)
	snapshotBefore := now.Add(-snapshotRetention)
	runBefore := now.Add(-runRetention)
	queries := []lifecycleQuery{
		{name: "automation_snapshots", count: `SELECT count(*) FROM automation_snapshots WHERE expires_at IS NOT NULL AND expires_at < $1`, apply: `DELETE FROM automation_snapshots WHERE expires_at IS NOT NULL AND expires_at < $1`, args: []any{snapshotBefore}},
		{name: "notification_deliveries", count: `SELECT count(*) FROM notification_deliveries WHERE status IN ('delivered','failed') AND updated_at < $1`, apply: `DELETE FROM notification_deliveries WHERE status IN ('delivered','failed') AND updated_at < $1`, args: []any{receiptBefore}},
		{name: "external_action_receipts", count: `SELECT count(*) FROM external_action_receipts WHERE status IN ('completed','failed') AND updated_at < $1`, apply: `DELETE FROM external_action_receipts WHERE status IN ('completed','failed') AND updated_at < $1`, args: []any{receiptBefore}},
		{name: "worker_llm_calls", count: `SELECT count(*) FROM worker_llm_calls WHERE status IN ('completed','failed') AND updated_at < $1`, apply: `DELETE FROM worker_llm_calls WHERE status IN ('completed','failed') AND updated_at < $1`, args: []any{receiptBefore}},
		{name: "domain_outbox_events", count: `SELECT count(*) FROM domain_outbox_events WHERE status IN ('done','failed') AND updated_at < $1`, apply: `DELETE FROM domain_outbox_events WHERE status IN ('done','failed') AND updated_at < $1`, args: []any{receiptBefore}},
		{name: "telegram_inbound_updates", count: `SELECT count(*) FROM telegram_inbound_updates WHERE status IN ('done','failed') AND updated_at < $1`, apply: `DELETE FROM telegram_inbound_updates WHERE status IN ('done','failed') AND updated_at < $1`, args: []any{telegramBefore}},
		{name: "telegram_delivery_parts", count: `SELECT count(*) FROM telegram_delivery_parts WHERE status IN ('delivered','failed') AND updated_at < $1`, apply: `DELETE FROM telegram_delivery_parts WHERE status IN ('delivered','failed') AND updated_at < $1`, args: []any{telegramBefore}},
		{
			name: "eino_checkpoints",
			count: `SELECT count(*) FROM eino_checkpoints c
				WHERE c.updated_at < $1
				  AND NOT EXISTS (SELECT 1 FROM eino_session_events e WHERE c.checkpoint_id='session/'||e.session_id||'/runner_checkpoint')
				  AND NOT EXISTS (SELECT 1 FROM chat_sessions s WHERE s.active AND c.checkpoint_id='session/'||s.engine_ref||'/runner_checkpoint')
				  AND NOT EXISTS (SELECT 1 FROM conversation_turns t WHERE t.status='running' AND c.checkpoint_id='session/'||t.engine_session||'/runner_checkpoint')`,
			apply: `DELETE FROM eino_checkpoints c
				WHERE c.updated_at < $1
				  AND NOT EXISTS (SELECT 1 FROM eino_session_events e WHERE c.checkpoint_id='session/'||e.session_id||'/runner_checkpoint')
				  AND NOT EXISTS (SELECT 1 FROM chat_sessions s WHERE s.active AND c.checkpoint_id='session/'||s.engine_ref||'/runner_checkpoint')
				  AND NOT EXISTS (SELECT 1 FROM conversation_turns t WHERE t.status='running' AND c.checkpoint_id='session/'||t.engine_session||'/runner_checkpoint')`,
			args: []any{runtimeBefore},
		},
		{
			name: "eino_session_events",
			count: `SELECT count(*) FROM eino_session_events e
				WHERE e.created_at < $1
				  AND NOT EXISTS (SELECT 1 FROM chat_sessions s WHERE s.active AND s.engine_ref=e.session_id)
				  AND NOT EXISTS (SELECT 1 FROM conversation_turns t WHERE t.status='running' AND t.engine_session=e.session_id)`,
			apply: `DELETE FROM eino_session_events e
				WHERE e.created_at < $1
				  AND NOT EXISTS (SELECT 1 FROM chat_sessions s WHERE s.active AND s.engine_ref=e.session_id)
				  AND NOT EXISTS (SELECT 1 FROM conversation_turns t WHERE t.status='running' AND t.engine_session=e.session_id)`,
			args: []any{runtimeBefore},
		},
		{name: "maintenance_runs", count: `SELECT count(*) FROM maintenance_runs WHERE status IN ('succeeded','failed') AND completed_at < $1 AND id NOT IN (SELECT last_run_id FROM maintenance_jobs WHERE last_run_id IS NOT NULL)`, apply: `DELETE FROM maintenance_runs WHERE status IN ('succeeded','failed') AND completed_at < $1 AND id NOT IN (SELECT last_run_id FROM maintenance_jobs WHERE last_run_id IS NOT NULL)`, args: []any{runBefore}},
	}
	return s.runLifecycleQueries(ctx, dryRun, queries)
}

type lifecycleQuery struct {
	name         string
	count, apply string
	args         []any
}

func (s *Store) runLifecycleQueries(ctx context.Context, dryRun bool, queries []lifecycleQuery, defaultArgs ...any) (LifecycleResult, error) {
	result := LifecycleResult{Details: make(map[string]int64, len(queries))}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, query := range queries {
		args := query.args
		if len(args) == 0 {
			args = defaultArgs
		}
		var candidates int64
		if err := tx.QueryRow(ctx, query.count, args...).Scan(&candidates); err != nil {
			return result, err
		}
		result.Inspected += candidates
		if dryRun || candidates == 0 {
			result.Details[query.name] = candidates
			continue
		}
		tag, err := tx.Exec(ctx, query.apply, args...)
		if err != nil {
			return result, err
		}
		reclaimed := tag.RowsAffected()
		result.Reclaimed += reclaimed
		result.Details[query.name] = reclaimed
	}
	if dryRun {
		return result, tx.Rollback(ctx)
	}
	return result, tx.Commit(ctx)
}
