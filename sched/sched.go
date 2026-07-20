// Package sched 是 DB 驱动的调度器：定时提醒投递、任务截止提醒/过期通知、
// AI 催办、每日待办汇总 + 超管全局日报、AI 周报。
// 状态全在库里（schedules、schedule_deliveries、automation_runs 与任务短租约），
// 进程随时可重启。
//
// 两级主动性：模板消息（提醒/通知/日报，确定性）与 AI 轮次（催办/周报——
// 调度器把系统指令注入用户会话跑一轮引擎，产出个性化内容后经 Notifier 推送）。
package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const (
	pollInterval       = 30 * time.Second
	kvDailySummary     = "daily_summary_last_day"
	kvWeeklyReport     = "weekly_report_last_week"
	kvProfileRefresh   = "profile_refresh_last_month"
	kvKnowledgeRefresh = "knowledge_refresh_last_month"
	kvOrphanNotice     = "orphan_notice_last_day"
	summaryMaxTasks    = 20
	// deadlineWarnWindow 截止前多久发临近提醒。
	deadlineWarnWindow = 24 * time.Hour
	// nudgeInterval 过期任务多久没有动静（过期通知/催办/进度更新）就 AI 催办一次。
	nudgeInterval = 48 * time.Hour
	// escalateAfter 累计催办达到该次数仍无进度时，通知分配者介入。
	escalateAfter = 2
	// digestOverdueLimit 日报中列出的过期任务上限。
	digestOverdueLimit = 10
	// orphanNoticeLimit 孤儿任务提醒中列出的明细上限。
	orphanNoticeLimit = 10
	// sendConcurrency 模板类推送/查询的并发上限（廉价、网络受限，可高于 AI）。
	sendConcurrency = 16
	messageTimeout  = 30 * time.Second
)

// Scheduler 调度器。
type Scheduler struct {
	store    *store.Store
	notifier notify.Notifier
	// orch 跑系统触发的 AI 轮次（催办/周报）；nil 时这两项能力关闭（测试或降级）。
	orch      *chat.Orchestrator
	bus       *events.Bus // 任务过期等事件交 AI 介入；nil 时回退模板（测试或降级）
	channel   string      // AI 结果按该渠道格式化并投递；执行上下文使用独立自动化会话
	tz        *time.Location
	dailyHour int // -1 关闭每日/每周汇总

	aiPool   *pool // AI 轮次限并发（护后端网关）：催办/周报/画像/定时 AI 推送
	sendPool *pool // 模板推送/逐人查询限并发（廉价）：每日汇总扇出
}

// New 创建调度器。aiConcurrency 是同时进行的 AI 轮次上限（<=0 取默认 4）。
func New(s *store.Store, n notify.Notifier, orch *chat.Orchestrator, bus *events.Bus, channel string, tz *time.Location, dailyHour, aiConcurrency int) *Scheduler {
	if aiConcurrency <= 0 {
		aiConcurrency = 4
	}
	return &Scheduler{
		store: s, notifier: n, orch: orch, bus: bus, channel: channel, tz: tz, dailyHour: dailyHour,
		aiPool: newPool(aiConcurrency), sendPool: newPool(sendConcurrency),
	}
}

// aiTurnTimeout 单个系统 AI 轮次的墙钟上限（与事件总线一致）。没有它，引擎
// 一次挂起会永久占用 aiPool 槽位并锁死该用户的 userLock——4 次挂起后全部
// AI 调度停摆、用户对话冻结。
const aiTurnTimeout = 4 * time.Minute

func (s *Scheduler) runAIReply(ctx context.Context, u *store.User, executionKey, directive, label string, readOnly bool) (string, error) {
	turnCtx, cancel := context.WithTimeout(ctx, aiTurnTimeout)
	defer cancel()
	var (
		reply string
		err   error
	)
	reply, err = s.orch.HandleAutomationMessage(turnCtx, u, s.channel, executionKey, directive, readOnly)
	if err != nil {
		slog.Error("定时 AI 轮次失败", "kind", label, "user", u.ID, "err", err)
		return "", err
	}
	return strings.TrimSpace(reply), nil
}

// runAIAndSend executes a read-only system report and then delivers it. It is
// safe to regenerate after a transient failure because the turn cannot mutate
// business state.
func (s *Scheduler) runAIAndSend(ctx context.Context, u *store.User, executionKey, directive, prefix, label string) bool {
	reply, err := s.runAIReply(ctx, u, executionKey, directive, label, true)
	if err != nil {
		return false
	}
	if reply == "" {
		slog.Warn("定时 AI 轮次返回空内容", "kind", label, "user", u.ID)
		return false
	}
	// Generation may consume nearly all of aiTurnTimeout. Delivery gets its own
	// bounded window so a valid result is not retried merely because only a few
	// milliseconds remained on the model context.
	return s.send(ctx, u.ID, prefix+reply)
}

// dispatchAI 派发一轮系统触发的 AI 生成 + 推送，受 aiPool 限并发、对 tick 非阻塞。
// prefix 前缀（如「🧭 月度人员盘点\n」），after 在推送成功后执行（可为 nil，用于催办升级）。
func (s *Scheduler) dispatchAI(ctx context.Context, u *store.User, executionKey, directive, prefix, label string, after func()) {
	s.aiPool.submit(ctx, func() {
		if s.runAIAndSend(ctx, u, executionKey, directive, prefix, label) && after != nil {
			after()
		}
	})
}

func (s *Scheduler) dispatchAutomationAI(ctx context.Context, run *store.AutomationRun, u *store.User, directive, prefix, label string) {
	if !s.aiPool.trySubmit(ctx, func() {
		reply := strings.TrimSpace(run.ResultText)
		if reply == "" {
			var err error
			reply, err = s.runAIReply(ctx, u, automationExecutionKey(run), directive, label, true)
			if err != nil {
				s.retryAutomation(ctx, run, label+"生成失败: "+err.Error())
				return
			}
			if reply = textfmt.TruncateRunes(strings.TrimSpace(reply), 12000); reply == "" {
				s.retryAutomation(ctx, run, label+"未生成可投递内容")
				return
			}
			if err := s.store.PrepareAutomationResult(ctx, run, reply); err != nil {
				s.retryAutomation(ctx, run, "保存"+label+"结果失败: "+err.Error())
				return
			}
			run.ResultText = reply
		}
		if !s.send(ctx, u.ID, prefix+reply) {
			s.retryAutomation(ctx, run, label+"投递失败")
			return
		}
		s.completeAutomation(ctx, run)
	}) {
		s.retryAutomation(ctx, run, "AI 执行池满载")
	}
}

// dispatchAutomationAction runs a scheduled maintenance agent that is allowed
// to write. The action boundary and generated report are durable: after the
// boundary a crash can only trigger a read-only state reconstruction, and a
// notification failure only resends the stored report.
func (s *Scheduler) dispatchAutomationAction(ctx context.Context, run *store.AutomationRun, u *store.User, directive, prefix, label string) {
	if !s.aiPool.trySubmit(ctx, func() {
		reply := strings.TrimSpace(run.ResultText)
		if reply == "" {
			var err error
			if run.ActionStarted {
				reply, err = s.runAIReply(ctx, u, automationExecutionKey(run), automationRecoveryDirective(directive), label+"恢复核对", true)
			} else {
				if err = s.store.BeginAutomationAction(ctx, run); err != nil {
					s.retryAutomation(ctx, run, "保存自动化动作边界失败: "+err.Error())
					return
				}
				run.ActionStarted = true
				reply, err = s.runAIReply(ctx, u, automationExecutionKey(run), directive, label, false)
				if err != nil {
					// The write-capable turn may have changed state before failing. Query
					// current facts once; never replay the original maintenance action.
					reply, err = s.runAIReply(ctx, u, automationExecutionKey(run), automationRecoveryDirective(directive), label+"恢复核对", true)
				}
			}
			if err != nil {
				reply = fmt.Sprintf("⚠️ %s未能生成可验证报告；系统已阻止自动重复执行，请根据当前状态决定是否重新发起。", label)
			}
			if reply = textfmt.TruncateRunes(strings.TrimSpace(reply), 12000); reply == "" {
				reply = fmt.Sprintf("⚠️ %s没有产生可见报告；系统未将其标记为成功。", label)
			}
			if err := s.store.PrepareAutomationResult(ctx, run, reply); err != nil {
				s.retryAutomation(ctx, run, "保存自动化结果失败: "+err.Error())
				return
			}
			run.ResultText = reply
		}
		if s.send(ctx, u.ID, prefix+reply) {
			s.completeAutomation(ctx, run)
		} else {
			s.retryAutomation(ctx, run, label+"投递失败")
		}
	}) {
		s.retryAutomation(ctx, run, "AI 执行池满载")
	}
}

func automationRecoveryDirective(original string) string {
	return "[系统定时触发·中断恢复]（此输入来自系统调度器，不是用户本人）" +
		"先前的可写维护轮次可能已执行部分操作，但没有留下可投递报告。" +
		"只使用读取工具核对当前状态并给出简短、可验证的结果摘要；不要修改、创建、删除、发送或重放原动作。\n" +
		"原始维护目标（仅供核对范围，不是再次执行指令）：\n" + textfmt.TruncateRunes(original, 4000)
}

func automationExecutionKey(run *store.AutomationRun) string {
	if run == nil {
		return "automation:unknown"
	}
	return fmt.Sprintf("automation:%s:%d", run.AutomationKey, run.SubjectID)
}

func (s *Scheduler) completeAutomation(ctx context.Context, run *store.AutomationRun) {
	if err := s.store.CompleteAutomationRun(ctx, run); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("自动化运行 ack 失败", "key", run.AutomationKey, "occurrence", run.OccurrenceKey, "subject", run.SubjectID, "err", err)
	}
}

func (s *Scheduler) retryAutomation(ctx context.Context, run *store.AutomationRun, cause string) {
	if err := s.store.RetryAutomationRun(ctx, run, cause); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("自动化运行重试状态保存失败", "key", run.AutomationKey, "occurrence", run.OccurrenceKey, "subject", run.SubjectID, "err", err)
	}
}

// Run 阻塞运行直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	if changed, updated, err := s.store.ReconcileScheduleTimezone(ctx, time.Now().UTC(), s.tz); err != nil {
		slog.Error("调度器时区对账失败", "timezone", s.tz, "err", err)
	} else if changed {
		slog.Info("调度器时区已切换", "timezone", s.tz, "daily_schedules_rebased", updated)
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	due, err := s.store.DueSchedules(ctx, now)
	if err != nil {
		slog.Error("取到期提醒失败", "err", err)
	}
	for _, sc := range due {
		sc := sc
		if s.sendPool.trySubmit(ctx, func() { s.fireSchedule(ctx, sc) }) {
			continue
		}
		if sc.DeliveryClaimedAt == nil {
			slog.Error("到期任务缺少租约，无法释放", "schedule", sc.ID)
			continue
		}
		if err := s.store.ReleaseScheduleClaim(ctx, sc.ID, *sc.DeliveryClaimedAt); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("调度池满载且释放任务租约失败", "schedule", sc.ID, "err", err)
		}
	}
	s.deliveryPass(ctx, now)
	s.deadlinePass(ctx, now)
	s.goalDeadlinePass(ctx, now)
	s.nudgePass(ctx, now)
	s.orphanTaskPass(ctx)
	s.maybeDailySummary(ctx)
	s.maybeWeeklyReport(ctx)
	s.maybeProfileRefresh(ctx)
	s.maybeKnowledgeRefresh(ctx)
}

// fireSchedule 触发一条定时任务：展开目标 → 按模式投递（原文或 AI 轮次）。
// daily 任务顺带把下次触发时间校正到工作日过滤后的正确时刻。
// 这里没有任何具体运营政策：几点、对谁、说什么全部来自数据行（AI 按对话创建）。
func (s *Scheduler) fireSchedule(ctx context.Context, sc *store.Schedule) {
	if sc.DeliveryClaimedAt == nil {
		slog.Error("拒绝执行缺少租约的定时任务", "schedule", sc.ID)
		return
	}
	claimAt := *sc.DeliveryClaimedAt
	now := time.Now().UTC()
	done := sc.Kind == store.ScheduleOnce
	var next *time.Time
	if sc.Kind == store.ScheduleDaily {
		n := store.NextDailyFire(now, sc.DailyAt, sc.Weekdays, s.tz)
		next = &n
		// 今天不在工作日过滤内（比如补跑/时钟漂移落到周末）：只校正不投递。
		if !dailyDeliveryAllowed(now, sc.Weekdays, s.tz) {
			if err := s.store.MarkScheduleDelivered(ctx, sc.ID, claimAt, now, next, false); err != nil {
				slog.Warn("daily 定时跳过校正失败", "schedule", sc.ID, "err", err)
			}
			return
		}
	} else if sc.Kind == store.ScheduleRepeat {
		n := nextRepeatFire(sc.FireAt, now, time.Duration(sc.IntervalS)*time.Second)
		next = &n
	}

	targets, err := s.resolveTargets(ctx, sc)
	if err != nil {
		slog.Warn("定时任务目标解析失败，保留租约等待重试", "schedule", sc.ID, "err", err)
		return
	}
	userIDs := make([]int64, 0, len(targets))
	for _, u := range targets {
		userIDs = append(userIDs, u.ID)
	}
	if err := s.store.FanOutScheduleOccurrence(ctx, sc, userIDs, now, next, done); err != nil {
		slog.Warn("定时任务逐接收人扇出失败，保留日程租约等待重试", "schedule", sc.ID, "err", err)
		return
	}
	slog.Info("定时任务已生成逐接收人投递", "schedule", sc.ID, "targets", len(userIDs), "occurrence", sc.FireAt)
	// fireSchedule 在 sendPool 中异步执行，当前 tick 的 deliveryPass 往往已经
	// 扫完；立即再扫一次，避免每次固定多等一个 30 秒轮询周期。
	s.deliveryPass(ctx, time.Now().UTC())
}

func dailyDeliveryAllowed(deliveryAt time.Time, weekdays string, tz *time.Location) bool {
	if tz == nil {
		tz = time.Local
	}
	return store.WeekdayAllowed(deliveryAt.In(tz).Weekday(), weekdays)
}

func (s *Scheduler) deliveryPass(ctx context.Context, now time.Time) {
	deliveries, err := s.store.DueScheduleDeliveries(ctx, now)
	if err != nil {
		slog.Error("取逐接收人定时投递失败", "err", err)
		return
	}
	for _, d := range deliveries {
		d := d
		if d.ClaimedAt == nil {
			continue
		}
		pool := s.deliveryPool(d.Mode)
		if pool.trySubmit(ctx, func() { s.deliverScheduleRecipient(ctx, d) }) {
			continue
		}
		if err := s.store.ReleaseScheduleDeliveryClaim(ctx, d.ID, *d.ClaimedAt); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("投递池满载且释放接收人租约失败", "delivery", d.ID, "err", err)
		}
	}
}

func (s *Scheduler) deliveryPool(mode string) *pool {
	if mode == store.ScheduleModeAI && s.orch != nil {
		return s.aiPool
	}
	return s.sendPool
}

func (s *Scheduler) deliverScheduleRecipient(ctx context.Context, d *store.ScheduleDelivery) {
	if d == nil || d.ClaimedAt == nil {
		return
	}
	u, err := s.store.UserByID(ctx, d.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.failScheduleRecipient(ctx, d, "接收人不存在")
		} else {
			s.retryScheduleRecipient(ctx, d, "读取接收人失败: "+err.Error())
		}
		return
	}
	if !humanRecipient(u) {
		s.failScheduleRecipient(ctx, d, "接收人已停用或不是人员用户")
		return
	}
	message := d.Message
	if d.Mode == store.ScheduleModeAI {
		message = strings.TrimSpace(d.ResultText)
		if message == "" && s.orch != nil {
			directive := s.scheduleAIDirective(ctx, d, time.Now())
			message, err = s.runAIReply(ctx, u, fmt.Sprintf("schedule:%d", d.ScheduleID), directive, "定时推送", true)
			if err != nil {
				s.retryScheduleRecipient(ctx, d, "生成定时推送失败: "+err.Error())
				return
			}
			if message = textfmt.TruncateRunes(strings.TrimSpace(message), 12000); message == "" {
				s.retryScheduleRecipient(ctx, d, "未生成可投递内容")
				return
			}
			if err := s.store.PrepareScheduleDeliveryResult(ctx, d.ID, *d.ClaimedAt, message); err != nil {
				s.retryScheduleRecipient(ctx, d, "保存定时推送结果失败: "+err.Error())
				return
			}
			d.ResultText = message
		}
		if message == "" {
			message = "⏰ 提醒：" + d.Message
		}
	} else {
		message = "⏰ 提醒：" + message
	}
	if !s.send(ctx, u.ID, message) {
		s.retryScheduleRecipient(ctx, d, "通知投递失败")
		return
	}
	if err := s.store.MarkScheduleDeliveryDelivered(ctx, d.ID, *d.ClaimedAt, time.Now().UTC()); err != nil {
		slog.Warn("接收人投递成功但 ack 失败", "delivery", d.ID, "err", err)
	}
}

func (s *Scheduler) scheduleAIDirective(ctx context.Context, d *store.ScheduleDelivery, generatedAt time.Time) string {
	tz := s.tz
	if tz == nil {
		tz = time.Local
	}
	authoredAt := d.CreatedAt
	var source *store.ChatMessage
	if sc, err := s.store.ScheduleByID(ctx, d.ScheduleID); err == nil {
		authoredAt = sc.CreatedAt
		if sc.SourceMessageID != nil {
			if message, err := s.store.ChatMessageByID(ctx, *sc.SourceMessageID); err == nil {
				source = message
			} else if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("读取日程来源消息失败", "schedule", d.ScheduleID, "message", *sc.SourceMessageID, "err", err)
			}
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("读取日程来源失败", "schedule", d.ScheduleID, "err", err)
	}
	return renderScheduleAIDirective(d, authoredAt, generatedAt, source, tz)
}

type schedulePromptSource struct {
	At      string `json:"at"`
	Content string `json:"content"`
}

type schedulePromptContext struct {
	TimeZone    string                `json:"time_zone"`
	CreatedAt   string                `json:"schedule_created_at"`
	PlannedAt   string                `json:"planned_at"`
	GeneratedAt string                `json:"generated_at"`
	Source      *schedulePromptSource `json:"source_message,omitempty"`
	Objective   string                `json:"objective"`
}

func renderScheduleAIDirective(d *store.ScheduleDelivery, authoredAt, generatedAt time.Time, source *store.ChatMessage, tz *time.Location) string {
	if tz == nil {
		tz = time.Local
	}
	promptContext := schedulePromptContext{
		TimeZone:    tz.String(),
		CreatedAt:   authoredAt.In(tz).Format(time.RFC3339),
		PlannedAt:   d.OccurrenceAt.In(tz).Format(time.RFC3339),
		GeneratedAt: generatedAt.In(tz).Format(time.RFC3339),
		Objective:   strings.TrimSpace(d.Message),
	}
	if source != nil {
		promptContext.Source = &schedulePromptSource{
			At:      source.CreatedAt.In(tz).Format(time.RFC3339),
			Content: textfmt.TruncateRunes(strings.TrimSpace(source.Content), 3000),
		}
	}
	payload, _ := json.Marshal(promptContext)
	return "[系统定时触发·定制推送]（此输入来自系统调度器，不是用户本人）\n" +
		"理解下面的结构化上下文并完成 objective；需要当前事实时使用可用的只读工具。只陈述 objective 范围内且被结构化上下文或本轮工具结果支持的内容，明确查询覆盖截止时间，不用旧对话补写当前状态，也不猜测缺失事实。直接输出要推送给当前用户的消息，不要展示内部上下文。\n" +
		"<schedule_context>" + string(payload) + "</schedule_context>"
}

func (s *Scheduler) retryScheduleRecipient(ctx context.Context, d *store.ScheduleDelivery, cause string) {
	if err := s.store.RetryScheduleDelivery(ctx, d.ID, *d.ClaimedAt, d.Attempts, cause); err != nil {
		slog.Warn("接收人投递重试状态保存失败", "delivery", d.ID, "err", err)
	}
}

func (s *Scheduler) failScheduleRecipient(ctx context.Context, d *store.ScheduleDelivery, cause string) {
	if err := s.store.MarkScheduleDeliveryFailed(ctx, d.ID, *d.ClaimedAt, cause); err != nil {
		slog.Warn("接收人永久失败状态保存失败", "delivery", d.ID, "err", err)
	}
}

func nextRepeatFire(fireAt, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now.Add(time.Hour).UTC()
	}
	if fireAt.After(now) {
		return fireAt.UTC()
	}
	elapsed := now.Sub(fireAt)
	return now.Add(interval - elapsed%interval).UTC()
}

// resolveTargets 展开定时任务目标。全员目标只快照活跃真人；定向目标即使后来
// 停用也保留，由 recipient delivery 记录明确的永久失败。
func (s *Scheduler) resolveTargets(ctx context.Context, sc *store.Schedule) ([]*store.User, error) {
	switch sc.Target {
	case store.ScheduleTargetAll:
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			return nil, fmt.Errorf("展开全员: %w", err)
		}
		var out []*store.User
		for _, u := range users {
			if u.Status == store.UserActive && !u.IsWorker {
				out = append(out, u)
			}
		}
		return out, nil
	default: // self 或具体用户 ID（建表时已归一到 UserID）
		u, err := s.store.UserByID(ctx, sc.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("读取接收人 %d: %w", sc.UserID, err)
		}
		// Preserve a recipient-level failed delivery for a user that became
		// inactive or changed type after the schedule was created. Silently
		// returning an empty target set would mark a one-shot schedule done with
		// no observable explanation.
		return []*store.User{u}, nil
	}
}

func humanRecipient(u *store.User) bool {
	return u != nil && u.Status == store.UserActive && !u.IsWorker
}

func (s *Scheduler) humanUser(ctx context.Context, userID int64) (*store.User, bool) {
	u, err := s.store.UserByID(ctx, userID)
	if err != nil || !humanRecipient(u) {
		return nil, false
	}
	return u, true
}

// nudgePass AI 催办：过期且长时间无动静的任务，跑一轮 AI 让它核实状态后
// 向执行人发出个性化催办。轮次挂在用户会话上，用户回复时 AI 记得自己问过什么。
func (s *Scheduler) nudgePass(ctx context.Context, now time.Time) {
	if s.orch == nil {
		return
	}
	due, err := s.store.DueNudges(ctx, now, nudgeInterval)
	if err != nil {
		slog.Error("取待催办任务失败", "err", err)
		return
	}
	for _, t := range due {
		if t.Deadline == nil {
			continue // 不应发生：过期通知必有截止时间；防御 panic 拖垮调度器
		}
		u, err := s.store.UserByID(ctx, t.AssigneeID)
		if err != nil || !humanRecipient(u) {
			continue
		}
		slog.Info("AI 催办启动", "task", t.ID, "user", u.ID, "nudge_count", t.NudgeCount)
		directive := s.nudgeDirective(ctx, t, u)
		t := t // 捕获循环变量
		// 催办成功后 ack；累计多次仍无进度 → 分配者介入（模板消息，确定性投递）。
		escalate := func() {
			if err := s.store.MarkNudgeSent(ctx, t.ID, time.Now().UTC()); err != nil {
				slog.Warn("催办 ack 失败", "task", t.ID, "err", err)
				return
			}
			if t.NudgeCount+1 >= escalateAfter && t.AssignerID != t.AssigneeID {
				s.send(ctx, t.AssignerID,
					fmt.Sprintf("⚠️ 你分配的任务「%s」（#%d，执行人 %s）已催办 %d 次仍无进度，请介入：调整任务、改期或改派。",
						t.Title, t.ID, u.Name, t.NudgeCount+1))
			}
		}
		s.dispatchAI(ctx, u, fmt.Sprintf("nudge:%d", t.ID), directive, "", "催办", escalate)
	}
}

// orphanTaskPass 每天把「执行人已停用但任务仍开放」的孤儿任务写入决策队列，并提醒分配者改派。
// 这是治理提醒，不自动改派：新执行人需要负责人根据任务上下文判断。
func (s *Scheduler) orphanTaskPass(ctx context.Context) {
	local := time.Now().In(s.tz)
	today := local.Format("2006-01-02")
	orphaned, err := s.store.OrphanedTasks(ctx)
	if err != nil {
		slog.Error("查询孤儿任务失败", "err", err)
		return
	}
	if len(orphaned) == 0 {
		return
	}
	byAssigner := map[int64][]*store.Task{}
	for _, t := range orphaned {
		byAssigner[t.AssignerID] = append(byAssigner[t.AssignerID], t)
		id := t.ID
		if _, err := s.store.UpsertDecisionItem(ctx, store.DecisionItem{
			OwnerID: t.AssignerID, Kind: "orphaned_task", Title: "改派孤儿任务：" + t.Title,
			Detail:  "执行人已停用，任务无人接手。建议用 reassign_task 改派给在线的 AI 员工或真人员工。",
			RefType: "task", RefID: &id, Priority: "high",
		}); err != nil {
			slog.Warn("写孤儿任务决策项失败", "task", t.ID, "owner", t.AssignerID, "err", err)
		}
	}
	for ownerID, tasks := range byAssigner {
		run, err := s.store.ClaimAutomationRun(ctx, kvOrphanNotice, today, ownerID, time.Now().UTC())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("认领孤儿任务提醒失败", "owner", ownerID, "err", err)
			}
			continue
		}
		owner, err := s.store.UserByID(ctx, ownerID)
		if err != nil || owner.Status != store.UserActive || owner.IsWorker {
			s.completeAutomation(ctx, run)
			continue
		}
		names := map[int64]string{}
		for _, t := range tasks {
			names[t.AssigneeID] = s.userName(ctx, t.AssigneeID)
		}
		msg := renderOrphanNotice(tasks, names)
		ownerID := ownerID
		if !s.sendPool.trySubmit(ctx, func() {
			if s.send(ctx, ownerID, msg) {
				s.completeAutomation(ctx, run)
			} else {
				s.retryAutomation(ctx, run, "孤儿任务提醒投递失败")
			}
		}) {
			s.retryAutomation(ctx, run, "通知执行池满载")
		}
	}
}

// maybeProfileRefresh 每月 1 号在配置小时，让 AI（以超管身份）基于任务履历
// 盘点成员并更新画像草稿——「对每个人的了解越来越准」的自动化落点。
func (s *Scheduler) maybeProfileRefresh(ctx context.Context) {
	if s.dailyHour < 0 || s.orch == nil {
		return
	}
	local := time.Now().In(s.tz)
	// 窗口放宽到 1-3 号：月度任务错过一小时的代价是整月，1 号恰逢发版/宕机
	// 不应导致静默跳过 30 天；kv 按月判重保证窗口内只跑一次。
	if local.Day() > 3 || local.Hour() != s.dailyHour {
		return
	}
	month := local.Format("2006-01")
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("画像盘点取用户失败", "err", err)
		return
	}
	directive := "[系统定时触发·月度人员盘点]（此输入来自系统调度器，不是用户本人）请做月度人员盘点：" +
		"用 list_users 取成员列表，逐个用 get_user_stats 看任务履历（负载、验收数、按时率、被催办情况），" +
		"结合 view_user_infos 里已有画像，用 save_infos_on_user 更新你名下对每位活跃成员的画像（整体替换，保留仍然成立的旧条目；无任务往来的成员跳过）。" +
		"画像条目写可执行的判断（擅长什么、可靠度、适合派什么任务），不写空话。" +
		"最后回复一份简短盘点摘要（每人一行）。若工具轮次不够用，优先覆盖任务量最大的成员，并在摘要里说明未覆盖谁。"
	for _, u := range users {
		if !u.IsSuperadmin || !humanRecipient(u) {
			continue
		}
		run, err := s.store.ClaimAutomationRun(ctx, kvProfileRefresh, month, u.ID, time.Now().UTC())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("认领画像盘点失败", "user", u.ID, "err", err)
			}
			continue
		}
		s.dispatchAutomationAction(ctx, run, u, directive, "🧭 月度人员盘点\n", "画像盘点")
	}
}

// maybeKnowledgeRefresh 每月 2 号在配置小时（错开 1 号的人员盘点），让 AI 以
// 超管身份整理知识库：合并重复、删过期、点名冲突——知识库要「越用越值钱」，
// 必须有代谢，否则积累两年后检索全是噪声。
func (s *Scheduler) maybeKnowledgeRefresh(ctx context.Context) {
	if s.dailyHour < 0 || s.orch == nil {
		return
	}
	local := time.Now().In(s.tz)
	// 窗口 2-4 号（错开 1 号的人员盘点），同样按月判重、错过首日不丢整月。
	if local.Day() < 2 || local.Day() > 4 || local.Hour() != s.dailyHour {
		return
	}
	month := local.Format("2006-01")
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("知识盘点取用户失败", "err", err)
		return
	}
	directive := "[系统定时触发·月度知识盘点]（此输入来自系统调度器，不是用户本人）请整理公司知识库：" +
		"用 list_recent_knowledge 与 search_knowledge 浏览近期与高频主题的条目，逐条判断：" +
		"重复的用 update_knowledge 合并到一条并 delete_knowledge 掉冗余；明显过期失效的删除；" +
		"内容互相矛盾的不要擅自定夺，在摘要里点名列出待用户裁决。" +
		"行为规则（list_rules）只检查是否与知识冲突，不要修改规则本身。" +
		"最后回复一份简短盘点报告：合并了什么、删了什么、发现哪些冲突；没有可整理的就说明知识库当前健康。"
	for _, u := range users {
		if !u.IsSuperadmin || !humanRecipient(u) {
			continue
		}
		run, err := s.store.ClaimAutomationRun(ctx, kvKnowledgeRefresh, month, 0, time.Now().UTC())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("认领知识盘点失败", "err", err)
			}
			return
		}
		if n, err := s.store.ScoreLearningCandidates(ctx, 500); err != nil {
			slog.Warn("学习候选治理评分失败", "err", err)
		} else if n > 0 {
			slog.Info("学习候选治理评分完成", "count", n)
		}
		s.dispatchAutomationAction(ctx, run, u, directive, "📚 月度知识盘点\n", "知识盘点")
		break // 知识库是全公司共享资产，一位超管盘一次即可，不必每位都跑
	}
}

// maybeWeeklyReport 每周一在配置小时，让 AI 用真实数据给每位超管写周报。
// automation_runs 以 ISO 周号和用户去重，重启不重发。
func (s *Scheduler) maybeWeeklyReport(ctx context.Context) {
	if s.dailyHour < 0 || s.orch == nil {
		return
	}
	local := time.Now().In(s.tz)
	if local.Weekday() != time.Monday || local.Hour() != s.dailyHour {
		return
	}
	year, week := local.ISOWeek()
	key := fmt.Sprintf("%d-W%02d", year, week)
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("周报取用户失败", "err", err)
		return
	}
	for _, u := range users {
		if !u.IsSuperadmin || !humanRecipient(u) {
			continue
		}
		run, err := s.store.ClaimAutomationRun(ctx, kvWeeklyReport, key, u.ID, time.Now().UTC())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("认领周报失败", "user", u.ID, "err", err)
			}
			continue
		}
		s.dispatchAutomationAI(ctx, run, u, s.weeklyReportDirective(local, u), "📈 每周汇总\n", "周报")
	}
}

func (s *Scheduler) nudgeDirective(ctx context.Context, t *store.Task, u *store.User) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[系统定时触发·催办]（此输入来自系统调度器，不是用户本人）任务 #%d「%s」已过截止时间（%s），且超过 %d 小时没有进度更新。\n",
		t.ID, t.Title, s.fmtTime(*t.Deadline), int(nudgeInterval.Hours()))
	b.WriteString("请先用工具核实该任务最新状态、进度、清单与分配关系；最终回复会作为主动消息直接推送给执行人（当前用户）。\n")
	b.WriteString("根据下面的情境自行决定语气、长度和结构：如果对方可能卡住，问清具体卡点；如果多次催办仍无进展，提醒影响并建议联系分配者调整任务、期限或资源。不要套固定模板。\n")
	fmt.Fprintf(&b, "任务类型：%s；累计催办：%d 次。\n", store.InferTaskKind(t.Title, t.Goal, t.Description, t.Acceptance, ""), t.NudgeCount)
	if st, err := s.store.StatsOfAssignee(ctx, u.ID); err == nil {
		fmt.Fprintf(&b, "执行人近期履历：在办 %d，当前过期 %d，待验收 %d，累计通过 %d", st.Open, st.OverdueNow, st.Awaiting, st.Accepted)
		if st.AcceptedWithDeadline > 0 {
			fmt.Fprintf(&b, "，按时 %d/%d", st.AcceptedOnTime, st.AcceptedWithDeadline)
		}
		b.WriteByte('\n')
	}
	kind := store.InferTaskKind(t.Title, t.Goal, t.Description, t.Acceptance, "")
	if out, err := s.store.TaskOutcomeStatsFor(ctx, u.ID, kind); err == nil && out.Total() > 0 {
		fmt.Fprintf(&b, "同类任务验收结果：通过 %d / 总计 %d。\n", out.Accepted, out.Total())
	}
	if ps, err := s.store.ProgressOf(ctx, t.ID); err == nil && len(ps) > 0 {
		b.WriteString("最近进度：\n")
		start := max(0, len(ps)-3)
		for _, p := range ps[start:] {
			fmt.Fprintf(&b, "- %s：%s\n", s.fmtTime(p.CreatedAt), textfmt.TruncateRunes(p.Content, 140))
		}
	}
	if profiles, err := s.store.ProfilesBy(ctx, u.ID, u.ID); err == nil && len(profiles) > 0 {
		b.WriteString("执行人自我画像：\n")
		for i, p := range profiles {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "- %s\n", textfmt.TruncateRunes(strings.TrimSpace(p.Content), 120))
		}
	}
	return b.String()
}

func (s *Scheduler) weeklyReportDirective(local time.Time, u *store.User) string {
	return fmt.Sprintf(
		"[系统定时触发·每周汇总]（此输入来自系统调度器，不是用户本人）今天是 %s 周一，请给老板写一份公司周报。"+
			"先用 company_overview 核实全局数据（含战略目标进度），需要细节时再用 view_goals、view_project、get_user_stats 等工具追查；一切以工具返回为准，不得编造。"+
			"根据老板画像和本周真实数据决定结构与详略；如果画像没有格式偏好，就用适合 Telegram 阅读的短分组呈现：进展、风险、建议动作。"+
			"避免模板腔，点名和建议必须能从工具数据或本轮上下文找到依据。你的回复会作为周报直接推送给 %s。",
		local.Format("2006-01-02"), u.Name)
}

// deadlinePass 任务临近截止提醒 + 过期通知。先 claim，投递成功后 ack；失败等租约过期重试。
func (s *Scheduler) deadlinePass(ctx context.Context, now time.Time) {
	warn, err := s.store.DueDeadlineReminders(ctx, now, deadlineWarnWindow)
	if err != nil {
		slog.Error("取临近截止任务失败", "err", err)
	}
	for _, t := range warn {
		t := t
		s.sendPool.submit(ctx, func() {
			_, ok := s.humanUser(ctx, t.AssigneeID)
			if ok && s.send(ctx, t.AssigneeID,
				fmt.Sprintf("⏳ 任务「%s」（#%d）将于 %s 截止，请安排进度。", t.Title, t.ID, s.fmtTime(*t.Deadline))) {
				if err := s.store.MarkDeadlineReminderSent(ctx, t.ID, time.Now().UTC()); err != nil {
					slog.Warn("临近截止提醒 ack 失败", "task", t.ID, "err", err)
				}
				return
			}
			if !ok {
				if err := s.store.MarkDeadlineReminderSent(ctx, t.ID, time.Now().UTC()); err != nil {
					slog.Warn("worker 临近截止提醒跳过 ack 失败", "task", t.ID, "err", err)
				}
			}
		})
	}

	over, err := s.store.DueOverdueNotices(ctx, now)
	if err != nil {
		slog.Error("取过期任务失败", "err", err)
	}
	for _, t := range over {
		t := t
		s.sendPool.submit(ctx, func() {
			_, human := s.humanUser(ctx, t.AssigneeID)
			ok := true
			if human {
				ok = s.send(ctx, t.AssigneeID,
					fmt.Sprintf("🔴 任务「%s」（#%d）已过截止时间（%s）。请立即更新进度，或与分配者沟通调整。", t.Title, t.ID, s.fmtTime(*t.Deadline)))
			}
			if t.AssignerID != t.AssigneeID {
				// 分配者侧走事件总线：AI 结合 worker 在线/任务状态决定是否改派或额外通知，
				// 而非死板模板。bus 未装配（测试）时回退原文模板，保证必达。
				detail := fmt.Sprintf("你分配的任务「%s」（#%d，执行人 %s）已过截止时间（%s）。",
					t.Title, t.ID, s.userName(ctx, t.AssigneeID), s.fmtTime(*t.Deadline))
				enqueued := s.bus != nil && s.bus.EnqueueRequired("任务过期", t.AssignerID, detail)
				if !enqueued && !s.send(ctx, t.AssignerID, "🔴 "+detail) {
					slog.Warn("过期通知分配者侧投递失败（不重试）", "task", t.ID, "assigner", t.AssignerID)
				}
			}
			if ok {
				if err := s.store.MarkOverdueNoticeSent(ctx, t.ID, time.Now().UTC()); err != nil {
					slog.Warn("过期通知 ack 失败", "task", t.ID, "err", err)
				}
			}
		})
	}
}

// goalDeadlinePass 战略目标临近截止提醒 + 过期通知。镜像 deadlinePass 的「先 claim，投递成功后 ack」，
// 但语义区别对待：目标快到期发模板提醒 owner（战略方向该定期看一眼）；目标过期走事件总线，
// 让 AI 自决是否点名相关人员、是否建议调整——目标是公司战略级，过期比单个任务更值得 AI 介入。
func (s *Scheduler) goalDeadlinePass(ctx context.Context, now time.Time) {
	warn, err := s.store.DueGoalDeadlineReminders(ctx, now, deadlineWarnWindow)
	if err != nil {
		slog.Error("取临近截止目标失败", "err", err)
	}
	for i := range warn {
		g := warn[i]
		if g.Deadline == nil {
			continue
		}
		s.sendPool.submit(ctx, func() {
			if _, ok := s.humanUser(ctx, g.OwnerID); !ok {
				if err := s.store.MarkGoalDeadlineReminderSent(ctx, g.ID, time.Now().UTC()); err != nil {
					slog.Warn("worker 目标临近截止提醒跳过 ack 失败", "goal", g.ID, "err", err)
				}
				return
			}
			if s.send(ctx, g.OwnerID,
				fmt.Sprintf("⏳ 战略目标「%s」将于 %s 到期，请确认里程碑进度是否需要调整。", g.Title, s.fmtTime(*g.Deadline))) {
				if err := s.store.MarkGoalDeadlineReminderSent(ctx, g.ID, time.Now().UTC()); err != nil {
					slog.Warn("目标临近截止提醒 ack 失败", "goal", g.ID, "err", err)
				}
			}
		})
	}

	over, err := s.store.DueGoalOverdueNotices(ctx, now)
	if err != nil {
		slog.Error("取过期目标失败", "err", err)
	}
	for i := range over {
		g := over[i]
		if g.Deadline == nil {
			continue
		}
		s.sendPool.submit(ctx, func() {
			if _, human := s.humanUser(ctx, g.OwnerID); !human {
				if err := s.store.MarkGoalOverdueNoticeSent(ctx, g.ID, time.Now().UTC()); err != nil {
					slog.Warn("worker 目标过期通知跳过 ack 失败", "goal", g.ID, "err", err)
				}
				return
			}
			// 事件先持久化再 ack；只有事件队列不可用时才回退同步模板，避免
			// 同一条逾期通知沿两条路径重复送达。
			detail := fmt.Sprintf("战略目标「%s」已过截止时间（%s）。请用 view_goals 核实里程碑进度，判断是否需要点名相关负责人、调整期限或重新拆解。",
				g.Title, s.fmtTime(*g.Deadline))
			ok := s.bus != nil && s.bus.EnqueueRequired("目标过期", g.OwnerID, detail)
			if !ok {
				ok = s.send(ctx, g.OwnerID, "🔴 "+detail)
			}
			if ok {
				if err := s.store.MarkGoalOverdueNoticeSent(ctx, g.ID, time.Now().UTC()); err != nil {
					slog.Warn("目标过期通知 ack 失败", "goal", g.ID, "err", err)
				}
			} else {
				slog.Warn("目标过期通知投递失败", "goal", g.ID, "owner", g.OwnerID)
			}
		})
	}

	// 里程碑截止提醒：里程碑无独立 owner，投给所属 goal 的 owner。
	// 里程碑卡住会连带阻塞整个目标的下游任务，比单个任务过期更值得点名。
	mwarn, err := s.store.DueMilestoneDeadlineReminders(ctx, now, deadlineWarnWindow)
	if err != nil {
		slog.Error("取临近截止里程碑失败", "err", err)
	}
	for i := range mwarn {
		m := mwarn[i]
		if m.Deadline == nil {
			continue
		}
		s.sendPool.submit(ctx, func() {
			ownerID, gTitle := s.milestoneOwner(ctx, m.GoalID)
			if ownerID == 0 {
				return // 目标已删，跳过
			}
			if _, ok := s.humanUser(ctx, ownerID); !ok {
				if err := s.store.MarkMilestoneDeadlineReminderSent(ctx, m.ID, time.Now().UTC()); err != nil {
					slog.Warn("worker 里程碑临近截止提醒跳过 ack 失败", "milestone", m.ID, "err", err)
				}
				return
			}
			if s.send(ctx, ownerID,
				fmt.Sprintf("⏳ 里程碑「%s」（属目标「%s」）将于 %s 到期。", m.Title, gTitle, s.fmtTime(*m.Deadline))) {
				if err := s.store.MarkMilestoneDeadlineReminderSent(ctx, m.ID, time.Now().UTC()); err != nil {
					slog.Warn("里程碑临近截止提醒 ack 失败", "milestone", m.ID, "err", err)
				}
			}
		})
	}

	mover, err := s.store.DueMilestoneOverdueNotices(ctx, now)
	if err != nil {
		slog.Error("取过期里程碑失败", "err", err)
	}
	for i := range mover {
		m := mover[i]
		if m.Deadline == nil {
			continue
		}
		s.sendPool.submit(ctx, func() {
			ownerID, gTitle := s.milestoneOwner(ctx, m.GoalID)
			if ownerID == 0 {
				return
			}
			if _, human := s.humanUser(ctx, ownerID); !human {
				if err := s.store.MarkMilestoneOverdueNoticeSent(ctx, m.ID, time.Now().UTC()); err != nil {
					slog.Warn("worker 里程碑过期通知跳过 ack 失败", "milestone", m.ID, "err", err)
				}
				return
			}
			// 与目标过期同构：优先进入持久事件队列，队列不可用才直发。
			detail := fmt.Sprintf("里程碑「%s」（属目标「%s」）已过截止时间（%s）。请用 get_milestone_detail 核实任务进度，点名停滞任务的执行人或重新拆解。",
				m.Title, gTitle, s.fmtTime(*m.Deadline))
			ok := s.bus != nil && s.bus.EnqueueRequired("里程碑过期", ownerID, detail)
			if !ok {
				ok = s.send(ctx, ownerID, "🔴 "+detail)
			}
			if ok {
				if err := s.store.MarkMilestoneOverdueNoticeSent(ctx, m.ID, time.Now().UTC()); err != nil {
					slog.Warn("里程碑过期通知 ack 失败", "milestone", m.ID, "err", err)
				}
			}
		})
	}
}

// milestoneOwner 解析里程碑所属 goal 的 owner 与标题。目标已删返回 (0, "")。
func (s *Scheduler) milestoneOwner(ctx context.Context, goalID int64) (int64, string) {
	g, err := s.store.GoalByID(ctx, goalID)
	if err != nil {
		return 0, ""
	}
	return g.OwnerID, g.Title
}

// maybeDailySummary 每天在配置小时给每个有待办的用户推送任务清单；
// 超管额外收到全局日报（老板视角：不追问也能掌握全局）。
// automation_runs 按日期和用户去重，重启不重发。
func (s *Scheduler) maybeDailySummary(ctx context.Context) {
	if s.dailyHour < 0 {
		return
	}
	local := time.Now().In(s.tz)
	if local.Hour() != s.dailyHour {
		return
	}
	today := local.Format("2006-01-02")
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("每日汇总取用户失败", "err", err)
		return
	}
	digest := s.buildDigest(ctx, users)
	// 逐人查待办 + 推送：受 sendPool 限并发异步扇出，几百上千人也不会串行拖成
	// 一长串，也不阻塞调度节拍（都是廉价 DB 查询 + 一条推送）。
	for _, u := range users {
		if !humanRecipient(u) {
			continue
		}
		u := u
		run, err := s.store.ClaimAutomationRun(ctx, kvDailySummary, today, u.ID, time.Now().UTC())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("认领每日汇总失败", "user", u.ID, "err", err)
			}
			continue
		}
		if !s.sendPool.trySubmit(ctx, func() {
			tasks, err := s.store.TasksOfAssignee(ctx, u.ID, true)
			if err != nil {
				slog.Warn("每日汇总取任务失败", "user", u.ID, "err", err)
				s.retryAutomation(ctx, run, err.Error())
				return
			}
			var parts []string
			if len(tasks) > 0 {
				parts = append(parts, renderTodos(tasks, s.tz))
			}
			if u.IsSuperadmin && digest != "" {
				parts = append(parts, digest)
			}
			if len(parts) == 0 || s.send(ctx, u.ID, strings.Join(parts, "\n\n")) {
				s.completeAutomation(ctx, run)
			} else {
				s.retryAutomation(ctx, run, "每日汇总投递失败")
			}
		}) {
			s.retryAutomation(ctx, run, "通知执行池满载")
		}
	}
}

// buildDigest 组装全局日报；没有值得说的内容时返回空串。
func (s *Scheduler) buildDigest(ctx context.Context, users []*store.User) string {
	stats, err := s.store.GlobalTaskStats(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		slog.Error("日报统计失败", "err", err)
		return ""
	}
	if stats.Open == 0 && stats.DoneSince == 0 {
		return ""
	}
	var overdue []*store.Task
	if stats.Overdue > 0 {
		if overdue, err = s.store.OverdueTasks(ctx, digestOverdueLimit); err != nil {
			slog.Error("日报过期任务查询失败", "err", err)
		}
	}
	names := make(map[int64]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Name
	}
	return renderDigest(stats, overdue, names, s.tz)
}

// renderTodos 个人待办清单（纯函数，可单测）。
func renderTodos(tasks []*store.Task, tz *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "☀️ 早上好，你今天有 %d 个待办：\n", len(tasks))
	for i, t := range tasks {
		if i >= summaryMaxTasks {
			fmt.Fprintf(&b, "…等共 %d 个\n", len(tasks))
			break
		}
		fmt.Fprintf(&b, "- #%d [%s] %s", t.ID, t.Status, t.Title)
		if t.Deadline != nil {
			fmt.Fprintf(&b, "（截止 %s）", t.Deadline.In(tz).Format("01-02 15:04"))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderDigest 超管全局日报（纯函数，可单测）。
func renderDigest(stats *store.TaskStats, overdue []*store.Task, names map[int64]string, tz *time.Location) string {
	var b strings.Builder
	b.WriteString("📊 全局概览\n")
	fmt.Fprintf(&b, "进行中任务 %d（其中已过期 %d）· 待验收 %d · 过去24小时验收通过 %d\n",
		stats.Open, stats.Overdue, stats.Awaiting, stats.DoneSince)
	if len(overdue) > 0 {
		b.WriteString("过期任务：\n")
		for _, t := range overdue {
			name := names[t.AssigneeID]
			if name == "" {
				name = fmt.Sprintf("用户%d", t.AssigneeID)
			}
			fmt.Fprintf(&b, "- #%d %s（执行人 %s，截止 %s）\n", t.ID, t.Title, name, t.Deadline.In(tz).Format("01-02 15:04"))
		}
		if int(stats.Overdue) > len(overdue) {
			fmt.Fprintf(&b, "…等共 %d 个\n", stats.Overdue)
		}
	}
	return b.String()
}

func renderOrphanNotice(tasks []*store.Task, names map[int64]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ 有 %d 个任务的执行人已停用，需要你改派：\n", len(tasks))
	for i, t := range tasks {
		if i >= orphanNoticeLimit {
			fmt.Fprintf(&b, "…等共 %d 个\n", len(tasks))
			break
		}
		name := names[t.AssigneeID]
		if name == "" {
			name = fmt.Sprintf("用户%d", t.AssigneeID)
		}
		fmt.Fprintf(&b, "- #%d %s（原执行人 %s）\n", t.ID, t.Title, name)
	}
	b.WriteString("请用 reassign_task 改派，进度历史会保留。")
	return b.String()
}

func (s *Scheduler) send(ctx context.Context, userID int64, text string) bool {
	sendCtx, cancel := context.WithTimeout(ctx, messageTimeout)
	defer cancel()
	if err := s.notifier.Send(sendCtx, userID, text); err != nil {
		slog.Warn("调度消息投递失败", "user", userID, "err", err)
		return false
	}
	slog.Info("调度消息已投递", "user", userID, "text_len", len(text))
	return true
}

func (s *Scheduler) userName(ctx context.Context, id int64) string {
	if u, err := s.store.UserByID(ctx, id); err == nil {
		return u.Name
	}
	return fmt.Sprintf("用户%d", id)
}

func (s *Scheduler) fmtTime(t time.Time) string {
	return t.In(s.tz).Format("2006-01-02 15:04")
}
