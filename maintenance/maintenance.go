// Package maintenance coordinates restart-safe lifecycle work without owning
// business deletion policy. Jobs are registered by their domain owners and
// only rebuildable derived data or provably temporary data may run automatically.
package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Class string

const (
	ClassDerived   Class = "derived"
	ClassEphemeral Class = "ephemeral"
	ClassAudit     Class = "audit"
	ClassAuthority Class = "authoritative"
)

const (
	TriggerAutomatic = "automatic"
	TriggerManual    = "manual"
)

var jobNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,95}$`)

var ErrJobBusy = errors.New("maintenance job is already running")

// Result is deliberately structural: domain jobs report stable dataset names,
// not sampled content or natural-language guesses about whether data is useful.
type Result struct {
	Inspected int64            `json:"inspected"`
	Reclaimed int64            `json:"reclaimed"`
	Bytes     int64            `json:"bytes,omitempty"`
	Details   map[string]int64 `json:"details,omitempty"`
	Notes     []string         `json:"notes,omitempty"`
}

type Job struct {
	Name        string
	Class       Class
	Description string
	Interval    time.Duration
	Timeout     time.Duration
	Run         func(context.Context, time.Time, bool) (Result, error)
}

type RunRecord struct {
	ID          int64      `json:"id"`
	JobName     string     `json:"job_name"`
	Class       Class      `json:"class"`
	Trigger     string     `json:"trigger"`
	DryRun      bool       `json:"dry_run"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Report      Result     `json:"report"`
	Error       string     `json:"error,omitempty"`
}

type JobState struct {
	Name            string     `json:"name"`
	Class           Class      `json:"class"`
	Description     string     `json:"description"`
	IntervalSeconds int64      `json:"interval_seconds"`
	NextRunAt       time.Time  `json:"next_run_at"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty"`
	LastStatus      string     `json:"last_status"`
	LastStartedAt   *time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt *time.Time `json:"last_completed_at,omitempty"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastReport      Result     `json:"last_report"`
	LastError       string     `json:"last_error,omitempty"`
	RunCount        int64      `json:"run_count"`
	FailureCount    int64      `json:"failure_count"`
}

type Status struct {
	Enabled  bool        `json:"enabled"`
	Running  bool        `json:"running"`
	Jobs     []JobState  `json:"jobs"`
	Runs     []RunRecord `json:"recent_runs"`
	Policies []Policy    `json:"policies"`
}

type Policy struct {
	Class     Class  `json:"class"`
	Automatic bool   `json:"automatic"`
	Rule      string `json:"rule"`
}

type Claim struct {
	RunID int64
	Job   Job
}

type Ledger interface {
	EnsureJobs(context.Context, []Job, time.Time) error
	Claim(context.Context, Job, string, string, bool, bool, time.Time) (*Claim, error)
	Finish(context.Context, Claim, string, bool, time.Time, Result, error) error
	Status(context.Context, int) ([]JobState, []RunRecord, error)
}

type Runner struct {
	ledger  Ledger
	jobs    map[string]Job
	owner   string
	enabled bool

	mu      sync.RWMutex
	running map[string]struct{}
}

func New(ledger Ledger, enabled bool, jobs ...Job) (*Runner, error) {
	if ledger == nil {
		return nil, errors.New("maintenance ledger is required")
	}
	registered := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		job.Name = strings.TrimSpace(job.Name)
		job.Description = strings.TrimSpace(job.Description)
		if !jobNameRE.MatchString(job.Name) {
			return nil, fmt.Errorf("invalid maintenance job name %q", job.Name)
		}
		if job.Class != ClassDerived && job.Class != ClassEphemeral {
			return nil, fmt.Errorf("maintenance job %s cannot automatically mutate %s data", job.Name, job.Class)
		}
		if job.Interval <= 0 || job.Run == nil {
			return nil, fmt.Errorf("maintenance job %s has invalid schedule or runner", job.Name)
		}
		if job.Timeout <= 0 {
			job.Timeout = min(job.Interval, 30*time.Minute)
		}
		if _, exists := registered[job.Name]; exists {
			return nil, fmt.Errorf("duplicate maintenance job %s", job.Name)
		}
		registered[job.Name] = job
	}
	return &Runner{
		ledger: ledger, jobs: registered, owner: randomOwner(), enabled: enabled,
		running: make(map[string]struct{}),
	}, nil
}

func (r *Runner) Run(ctx context.Context) {
	if r == nil || !r.enabled {
		return
	}
	if err := r.ensure(ctx); err != nil {
		slog.Warn("初始化数据生命周期任务失败，将继续重试", "err", err)
	}
	r.runDue(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := r.ensure(ctx); err != nil {
			slog.Warn("同步数据生命周期任务失败", "err", err)
			continue
		}
		r.runDue(ctx)
	}
}

func (r *Runner) RunNow(ctx context.Context, names []string, dryRun bool) ([]RunRecord, error) {
	if r == nil || !r.enabled {
		return nil, errors.New("数据生命周期维护未启用")
	}
	selected, err := r.selectJobs(names)
	if err != nil {
		return nil, err
	}
	if err := r.ensure(ctx); err != nil {
		return nil, err
	}
	records := make([]RunRecord, 0, len(selected))
	var runErrors []error
	for _, job := range selected {
		record, claimed, err := r.runOne(ctx, job, TriggerManual, dryRun, true)
		if err != nil {
			runErrors = append(runErrors, fmt.Errorf("%s: %w", job.Name, err))
		}
		if claimed {
			records = append(records, record)
		}
	}
	return records, errors.Join(runErrors...)
}

func (r *Runner) Status(ctx context.Context) (Status, error) {
	if r == nil {
		return Status{}, nil
	}
	jobs, runs, err := r.ledger.Status(ctx, 30)
	if err != nil {
		return Status{}, err
	}
	jobs = slices.DeleteFunc(jobs, func(job JobState) bool {
		_, registered := r.jobs[job.Name]
		return !registered
	})
	r.mu.RLock()
	running := len(r.running) > 0
	r.mu.RUnlock()
	if !running {
		now := time.Now().UTC()
		running = slices.ContainsFunc(jobs, func(job JobState) bool {
			return job.LeaseUntil != nil && job.LeaseUntil.After(now)
		})
	}
	return Status{
		Enabled: r.enabled, Running: running, Jobs: jobs, Runs: runs,
		Policies: []Policy{
			{Class: ClassAuthority, Automatic: false, Rule: "业务事实只归档或由明确领域操作变更，维护服务不删除"},
			{Class: ClassAudit, Automatic: false, Rule: "审计事实默认保留；只有独立保留策略能处理其可重建载荷"},
			{Class: ClassDerived, Automatic: true, Rule: "仅依据权威稳定 ID 对账后重建或回收"},
			{Class: ClassEphemeral, Automatic: true, Rule: "仅在用途已终结且超过配置保留期后回收"},
		},
	}, nil
}

func (r *Runner) ensure(ctx context.Context) error {
	jobs := make([]Job, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, job)
	}
	slices.SortFunc(jobs, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
	return r.ledger.EnsureJobs(ctx, jobs, time.Now().UTC())
}

func (r *Runner) runDue(ctx context.Context) {
	jobs, _ := r.selectJobs(nil)
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if _, _, err := r.runOne(ctx, job, TriggerAutomatic, false, false); err != nil && ctx.Err() == nil {
			slog.Warn("数据生命周期任务调度失败", "job", job.Name, "err", err)
		}
	}
}

func (r *Runner) runOne(parent context.Context, job Job, trigger string, dryRun, force bool) (record RunRecord, claimed bool, err error) {
	now := time.Now().UTC()
	claim, err := r.ledger.Claim(parent, job, r.owner, trigger, dryRun, force, now)
	if err != nil {
		return record, false, err
	}
	if claim == nil {
		if force {
			return record, false, ErrJobBusy
		}
		return record, false, nil
	}
	claimed = true
	r.mu.Lock()
	r.running[job.Name] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, job.Name)
		r.mu.Unlock()
	}()

	runCtx, cancel := context.WithTimeout(parent, job.Timeout)
	defer cancel()
	var result Result
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
			}
		}()
		result, err = job.Run(runCtx, now, dryRun)
	}()
	finished := time.Now().UTC()
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer finishCancel()
	if finishErr := r.ledger.Finish(finishCtx, *claim, r.owner, dryRun, finished, result, err); finishErr != nil {
		err = errors.Join(err, fmt.Errorf("记录维护结果: %w", finishErr))
	}
	status := "succeeded"
	errText := ""
	if err != nil {
		status = "failed"
		errText = err.Error()
		slog.Warn("数据生命周期任务失败", "job", job.Name, "dry_run", dryRun, "err", err)
	} else {
		slog.Info("数据生命周期任务完成", "job", job.Name, "dry_run", dryRun,
			"inspected", result.Inspected, "reclaimed", result.Reclaimed, "bytes", result.Bytes)
	}
	record = RunRecord{ID: claim.RunID, JobName: job.Name, Class: job.Class, Trigger: trigger,
		DryRun: dryRun, Status: status, StartedAt: now, CompletedAt: &finished, Report: result, Error: errText}
	return record, true, err
}

func (r *Runner) selectJobs(names []string) ([]Job, error) {
	if len(names) == 0 {
		jobs := make([]Job, 0, len(r.jobs))
		for _, job := range r.jobs {
			jobs = append(jobs, job)
		}
		slices.SortFunc(jobs, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
		return jobs, nil
	}
	seen := make(map[string]struct{}, len(names))
	jobs := make([]Job, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		job, ok := r.jobs[name]
		if !ok {
			return nil, fmt.Errorf("未知维护任务 %q", name)
		}
		seen[name] = struct{}{}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func randomOwner() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("maintenance-%d", time.Now().UnixNano())
	}
	return "maintenance-" + hex.EncodeToString(raw)
}

func marshalResult(result Result) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}
