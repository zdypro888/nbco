package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	TaskOutcomeAccepted = "accepted"
	TaskOutcomeRejected = "rejected"

	TaskKindEngineering   = "engineering"
	TaskKindMaterials     = "materials"
	TaskKindReview        = "review"
	TaskKindResearch      = "research"
	TaskKindOperations    = "operations"
	TaskKindProductDesign = "product_design"
	TaskKindGeneral       = "general"
)

const taskOutcomeReasonMax = 800

// TaskOutcome records one review result. It is the durable feedback loop used
// by worker dispatch and proactive prompts; it does not replace task_progress,
// which remains the human-readable audit trail.
type TaskOutcome struct {
	ID         int64
	TaskID     int64
	AssigneeID int64
	ReviewerID int64
	Outcome    string
	TaskKind   string
	Reason     string
	CreatedAt  time.Time
}

type TaskOutcomeInput struct {
	TaskID     int64
	AssigneeID int64
	ReviewerID int64
	Outcome    string
	TaskKind   string
	Reason     string
}

type TaskOutcomeStats struct {
	Accepted int64
	Rejected int64
}

func (s TaskOutcomeStats) Total() int64 { return s.Accepted + s.Rejected }

func (s TaskOutcomeStats) PassRate() float64 {
	if s.Total() == 0 {
		return 0
	}
	return float64(s.Accepted) / float64(s.Total())
}

// RecordTaskOutcome appends one structured task review outcome. Callers should
// treat failures as telemetry failures after the task state transition has
// already happened, not roll business state back retroactively.
func (s *Store) RecordTaskOutcome(ctx context.Context, in TaskOutcomeInput) error {
	outcome := strings.TrimSpace(strings.ToLower(in.Outcome))
	if outcome != TaskOutcomeAccepted && outcome != TaskOutcomeRejected {
		return fmt.Errorf("invalid task outcome %q", in.Outcome)
	}
	kind := NormalizeTaskKind(in.TaskKind)
	if kind == "" {
		kind = "general"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_outcomes (task_id, assignee_id, reviewer_id, outcome, task_kind, reason)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		in.TaskID, in.AssigneeID, in.ReviewerID, outcome, kind, truncateRunes(strings.TrimSpace(in.Reason), taskOutcomeReasonMax))
	return wrapErr(err)
}

// TaskOutcomeStatsFor returns review outcomes for an assignee. When taskKind is
// empty it returns all task kinds; otherwise it returns that kind only.
func (s *Store) TaskOutcomeStatsFor(ctx context.Context, assigneeID int64, taskKind string) (*TaskOutcomeStats, error) {
	var st TaskOutcomeStats
	kind := NormalizeTaskKind(taskKind)
	var err error
	if kind == "" {
		err = s.pool.QueryRow(ctx,
			`SELECT
			   count(*) FILTER (WHERE outcome = 'accepted'),
			   count(*) FILTER (WHERE outcome = 'rejected')
			 FROM task_outcomes WHERE assignee_id = $1`, assigneeID).
			Scan(&st.Accepted, &st.Rejected)
	} else {
		err = s.pool.QueryRow(ctx,
			`SELECT
			   count(*) FILTER (WHERE outcome = 'accepted'),
			   count(*) FILTER (WHERE outcome = 'rejected')
			 FROM task_outcomes WHERE assignee_id = $1 AND task_kind = $2`, assigneeID, kind).
			Scan(&st.Accepted, &st.Rejected)
	}
	return &st, err
}

func NormalizeTaskKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case TaskKindEngineering, TaskKindMaterials, TaskKindReview, TaskKindResearch,
		TaskKindOperations, TaskKindProductDesign, TaskKindGeneral:
		return k
	case "":
		return ""
	default:
		return TaskKindGeneral
	}
}

func IsTaskKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case TaskKindEngineering, TaskKindMaterials, TaskKindReview, TaskKindResearch,
		TaskKindOperations, TaskKindProductDesign, TaskKindGeneral:
		return true
	default:
		return false
	}
}
