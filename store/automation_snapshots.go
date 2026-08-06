package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// AutomationSnapshot freezes the membership of one recurring maintenance
// cycle. Retry eligibility belongs to automation_runs; this record only owns
// the immutable input boundary.
type AutomationSnapshot struct {
	AutomationKey string
	OccurrenceKey string
	SubjectID     int64
	ItemKind      string
	ItemIDs       []int64
	ExpiresAt     *time.Time
	CreatedAt     time.Time
}

const automationSnapshotCols = `automation_key, occurrence_key, subject_id, item_kind, item_ids, expires_at, created_at`

func scanAutomationSnapshot(row interface{ Scan(...any) error }) (*AutomationSnapshot, error) {
	var snapshot AutomationSnapshot
	if err := row.Scan(&snapshot.AutomationKey, &snapshot.OccurrenceKey, &snapshot.SubjectID,
		&snapshot.ItemKind, &snapshot.ItemIDs, &snapshot.ExpiresAt, &snapshot.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &snapshot, nil
}

// GetOrCreateAutomationSnapshot atomically opens a cycle. Once inserted, its
// item kind and IDs never change, even when a later retry supplies newer rows.
func (s *Store) GetOrCreateAutomationSnapshot(
	ctx context.Context,
	automationKey, occurrenceKey string,
	subjectID int64,
	itemKind string,
	itemIDs []int64,
	expiresAt time.Time,
) (*AutomationSnapshot, error) {
	automationKey = strings.TrimSpace(automationKey)
	occurrenceKey = strings.TrimSpace(occurrenceKey)
	itemKind = strings.TrimSpace(itemKind)
	if automationKey == "" || occurrenceKey == "" || itemKind == "" {
		return nil, ErrNotFound
	}
	itemIDs = normalizeSnapshotIDs(itemIDs)
	var expiry any
	if !expiresAt.IsZero() {
		expiry = expiresAt.UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := scanAutomationSnapshot(tx.QueryRow(ctx,
		`SELECT `+automationSnapshotCols+` FROM automation_snapshots
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`,
		automationKey, occurrenceKey, subjectID))
	if err == nil {
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO automation_snapshots
		       (automation_key, occurrence_key, subject_id, item_kind, item_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (automation_key, occurrence_key, subject_id) DO NOTHING`,
		automationKey, occurrenceKey, subjectID, itemKind, itemIDs, expiry); err != nil {
		return nil, err
	}
	snapshot, err := scanAutomationSnapshot(tx.QueryRow(ctx,
		`SELECT `+automationSnapshotCols+` FROM automation_snapshots
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`,
		automationKey, occurrenceKey, subjectID))
	if err != nil {
		return nil, err
	}
	return snapshot, tx.Commit(ctx)
}

func normalizeSnapshotIDs(ids []int64) []int64 {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return slices.DeleteFunc(ids, func(id int64) bool { return id <= 0 })
}

// DeleteExpiredAutomationSnapshots bounds the retained snapshot ledger. Runs
// keep their own audit trail; an expired input membership is no longer needed.
func (s *Store) DeleteExpiredAutomationSnapshots(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM automation_snapshots WHERE expires_at IS NOT NULL AND expires_at < $1`, before)
	if err != nil {
		return 0, wrapErr(err)
	}
	return tag.RowsAffected(), nil
}
