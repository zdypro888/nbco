package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TaskOutcomeAccepted = "accepted"
	TaskOutcomeRejected = "rejected"
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

var taskKindWordRe = regexp.MustCompile(`[a-z0-9]+|[\p{Han}]+`)

// InferTaskKind maps free-form task text to a small, stable taxonomy. This is
// intentionally generic: it gives the learning loop dimensions without encoding
// one-off business policy into code.
func InferTaskKind(parts ...string) string {
	text := strings.ToLower(strings.Join(parts, "\n"))
	if strings.TrimSpace(text) == "" {
		return "general"
	}
	words := taskKindWordRe.FindAllString(text, -1)
	has := func(keys ...string) bool {
		for _, key := range keys {
			key = strings.ToLower(key)
			for _, w := range words {
				if w == key || strings.Contains(w, key) || strings.Contains(key, w) && utf8.RuneCountInString(w) >= 2 {
					return true
				}
			}
			if strings.Contains(text, key) {
				return true
			}
		}
		return false
	}
	switch {
	case has("go", "python", "typescript", "javascript", "代码", "开发", "bug", "接口", "api", "部署", "升级", "git", "codex", "claude"):
		return "engineering"
	case has("pdf", "xlsx", "excel", "csv", "表格", "资料", "文件", "整理", "提取", "归档", "照片", "图片"):
		return "materials"
	case has("测试", "验收", "qa", "review", "审查", "审计", "回归", "用例"):
		return "review"
	case has("调研", "搜索", "竞品", "研究", "分析", "报告", "方案"):
		return "research"
	case has("通知", "提醒", "值日", "排班", "行政", "人事", "档案", "合同", "账单"):
		return "operations"
	case has("前端", "ui", "ux", "设计", "页面", "mini app", "html", "css"):
		return "product_design"
	default:
		return "general"
	}
}

func NormalizeTaskKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "engineering", "materials", "review", "research", "operations", "product_design", "general":
		return k
	case "":
		return ""
	default:
		return "general"
	}
}
