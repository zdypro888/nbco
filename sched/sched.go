// Package sched 是 DB 驱动的调度器：定时提醒投递、任务截止提醒/过期通知、
// AI 催办、每日待办汇总 + 超管全局日报、AI 周报。
// 状态全在库里（schedules 表 + tasks 发送标记/短租约 + kv_state），进程随时可重启。
//
// 两级主动性：模板消息（提醒/通知/日报，确定性）与 AI 轮次（催办/周报——
// 调度器把系统指令注入用户会话跑一轮引擎，产出个性化内容后经 Notifier 推送）。
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/store"
)

const (
	pollInterval       = 30 * time.Second
	kvDailySummary     = "daily_summary_last_day"
	kvWeeklyReport     = "weekly_report_last_week"
	kvProfileRefresh   = "profile_refresh_last_month"
	kvKnowledgeRefresh = "knowledge_refresh_last_month"
	summaryMaxTasks    = 20
	// deadlineWarnWindow 截止前多久发临近提醒。
	deadlineWarnWindow = 24 * time.Hour
	// nudgeInterval 过期任务多久没有动静（过期通知/催办/进度更新）就 AI 催办一次。
	nudgeInterval = 48 * time.Hour
	// escalateAfter 累计催办达到该次数仍无进度时，通知分配者介入。
	escalateAfter = 2
	// digestOverdueLimit 日报中列出的过期任务上限。
	digestOverdueLimit = 10
	// sendConcurrency 模板类推送/查询的并发上限（廉价、网络受限，可高于 AI）。
	sendConcurrency = 16
)

// Scheduler 调度器。
type Scheduler struct {
	store    *store.Store
	notifier notify.Notifier
	// orch 跑系统触发的 AI 轮次（催办/周报）；nil 时这两项能力关闭（测试或降级）。
	orch      *chat.Orchestrator
	bus       *events.Bus // 任务过期等事件交 AI 介入；nil 时回退模板（测试或降级）
	channel   string // AI 轮次挂在用户哪个渠道的会话上（与主入口一致，保证上下文连续）
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

// runAIAndSend 同步执行一轮系统触发的 AI 生成 + 推送，返回是否完整成功。
func (s *Scheduler) runAIAndSend(ctx context.Context, u *store.User, directive, prefix, label string) bool {
	tctx, cancel := context.WithTimeout(ctx, aiTurnTimeout)
	defer cancel()
	reply, err := s.orch.HandleMessage(tctx, u, s.channel, directive)
	if err != nil {
		slog.Error("定时 AI 轮次失败", "kind", label, "user", u.ID, "err", err)
		return false
	}
	return s.send(ctx, u.ID, prefix+reply)
}

// dispatchAI 派发一轮系统触发的 AI 生成 + 推送，受 aiPool 限并发、对 tick 非阻塞。
// prefix 前缀（如「🧭 月度人员盘点\n」），after 在推送成功后执行（可为 nil，用于催办升级）。
func (s *Scheduler) dispatchAI(ctx context.Context, u *store.User, directive, prefix, label string, after func()) {
	s.aiPool.submit(ctx, func() {
		if s.runAIAndSend(ctx, u, directive, prefix, label) && after != nil {
			after()
		}
	})
}

// Run 阻塞运行直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
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
		s.schedulePool(sc).submit(ctx, func() { s.fireSchedule(ctx, sc) })
	}
	s.deadlinePass(ctx, now)
	s.nudgePass(ctx, now)
	s.maybeDailySummary(ctx)
	s.maybeWeeklyReport(ctx)
	s.maybeProfileRefresh(ctx)
	s.maybeKnowledgeRefresh(ctx)
}

func (s *Scheduler) schedulePool(sc *store.Schedule) *pool {
	if sc.Mode == store.ScheduleModeAI && s.orch != nil {
		return s.aiPool
	}
	return s.sendPool
}

// fireSchedule 触发一条定时任务：展开目标 → 按模式投递（原文或 AI 轮次）。
// daily 任务顺带把下次触发时间校正到工作日过滤后的正确时刻。
// 这里没有任何具体运营政策：几点、对谁、说什么全部来自数据行（AI 按对话创建）。
func (s *Scheduler) fireSchedule(ctx context.Context, sc *store.Schedule) {
	now := time.Now().UTC()
	done := sc.Kind == store.ScheduleOnce
	var next *time.Time
	if sc.Kind == store.ScheduleDaily {
		n := store.NextDailyFire(now, sc.DailyAt, sc.Weekdays, s.tz)
		next = &n
		// 今天不在工作日过滤内（比如补跑/时钟漂移落到周末）：只校正不投递。
		if !store.WeekdayAllowed(time.Now().In(s.tz).Weekday(), sc.Weekdays) {
			if err := s.store.MarkScheduleDelivered(ctx, sc.ID, now, next, false); err != nil {
				slog.Warn("daily 定时跳过校正失败", "schedule", sc.ID, "err", err)
			}
			return
		}
	} else if sc.Kind == store.ScheduleRepeat {
		n := nextRepeatFire(sc.FireAt, now, time.Duration(sc.IntervalS)*time.Second)
		next = &n
	}

	targets := s.resolveTargets(ctx, sc)
	sent, failed := 0, 0
	for _, u := range targets {
		var one bool
		switch sc.Mode {
		case store.ScheduleModeAI:
			if s.orch == nil {
				one = s.send(ctx, u.ID, "⏰ "+sc.Message)
				break
			}
			directive := fmt.Sprintf(
				"[系统定时触发·定制推送]（此输入来自系统调度器，不是用户本人）请按以下指令产出要推送给当前用户的内容，"+
					"需要事实（如其今日待办、任务状态）先用工具查询，个性化、简洁、不编造：\n%s\n"+
					"你的回复会作为主动消息直接推送给该用户。", sc.Message)
			one = s.runAIAndSend(ctx, u, directive, "", "定时推送")
		default:
			one = s.send(ctx, u.ID, "⏰ 提醒："+sc.Message)
		}
		if one {
			sent++
		} else {
			failed++
		}
	}
	// ack 语义：只要不是「全员失败」就推进。全成功才 ack 的旧语义下，多目标
	// 任务里任一永久不可达用户（受邀未绑 TG、拉黑 bot）会让 fire_at 永不推进、
	// 租约过期后对所有可达用户无限重发——once 变永动机，AI 模式还重复烧 token。
	// 部分失败者本次丢失该条推送（记日志），比无限轰炸其他人代价小得多。
	if sent > 0 || len(targets) == 0 {
		if failed > 0 {
			slog.Warn("定时任务部分目标投递失败（不重试，避免对已达目标重复推送）",
				"schedule", sc.ID, "sent", sent, "failed", failed)
		}
		if err := s.store.MarkScheduleDelivered(ctx, sc.ID, now, next, done); err != nil {
			slog.Warn("定时任务 ack 失败", "schedule", sc.ID, "err", err)
		}
		return
	}
	slog.Warn("定时任务全部目标投递失败，等待租约过期后重试", "schedule", sc.ID, "targets", len(targets))
}

func nextRepeatFire(fireAt, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now.Add(time.Hour).UTC()
	}
	next := fireAt
	for !next.After(now) {
		next = next.Add(interval)
	}
	return next.UTC()
}

// resolveTargets 展开定时任务的目标为具体用户（活跃、非 worker）。
func (s *Scheduler) resolveTargets(ctx context.Context, sc *store.Schedule) []*store.User {
	switch sc.Target {
	case store.ScheduleTargetAll:
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			slog.Error("定时任务展开全员失败", "schedule", sc.ID, "err", err)
			return nil
		}
		var out []*store.User
		for _, u := range users {
			if u.Status == store.UserActive && !u.IsWorker {
				out = append(out, u)
			}
		}
		return out
	default: // self 或具体用户 ID（建表时已归一到 UserID）
		u, err := s.store.UserByID(ctx, sc.UserID)
		if err != nil || u.Status != store.UserActive {
			return nil
		}
		return []*store.User{u}
	}
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
		if err != nil || u.Status != store.UserActive {
			continue
		}
		slog.Info("AI 催办启动", "task", t.ID, "user", u.ID, "nudge_count", t.NudgeCount)
		directive := fmt.Sprintf(
			"[系统定时触发·催办]（此输入来自系统调度器，不是用户本人）任务 #%d「%s」已过截止时间（%s），且超过 %d 小时没有进度更新。"+
				"请先用工具核实该任务的最新状态、进度与清单，然后直接向执行人（当前用户）发出催办："+
				"语气友善但明确，询问具体卡点，提醒截止影响；若对方确有困难，建议其联系分配者调整任务或期限。"+
				"你的回复会作为主动消息直接推送给执行人。",
			t.ID, t.Title, s.fmtTime(*t.Deadline), int(nudgeInterval.Hours()))
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
		s.dispatchAI(ctx, u, directive, "", "催办", escalate)
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
	last, err := s.store.GetKV(ctx, kvProfileRefresh)
	if err != nil {
		slog.Error("读画像盘点状态失败", "err", err)
		return
	}
	if last == month {
		return
	}
	if err := s.store.SetKV(ctx, kvProfileRefresh, month); err != nil {
		slog.Error("写画像盘点状态失败", "err", err)
		return
	}
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
		if !u.IsSuperadmin || u.Status != store.UserActive {
			continue
		}
		s.dispatchAI(ctx, u, directive, "🧭 月度人员盘点\n", "画像盘点", nil)
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
	last, err := s.store.GetKV(ctx, kvKnowledgeRefresh)
	if err != nil {
		slog.Error("读知识盘点状态失败", "err", err)
		return
	}
	if last == month {
		return
	}
	if err := s.store.SetKV(ctx, kvKnowledgeRefresh, month); err != nil {
		slog.Error("写知识盘点状态失败", "err", err)
		return
	}
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
		if !u.IsSuperadmin || u.Status != store.UserActive {
			continue
		}
		s.dispatchAI(ctx, u, directive, "📚 月度知识盘点\n", "知识盘点", nil)
		break // 知识库是全公司共享资产，一位超管盘一次即可，不必每位都跑
	}
}

// maybeWeeklyReport 每周一在配置小时，让 AI 用真实数据给每位超管写周报。
// kv_state 记 ISO 周号，重启不重发。
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
	last, err := s.store.GetKV(ctx, kvWeeklyReport)
	if err != nil {
		slog.Error("读周报状态失败", "err", err)
		return
	}
	if last == key {
		return
	}
	// 先写状态再发送：宁可漏发一周，不可无限重发。
	if err := s.store.SetKV(ctx, kvWeeklyReport, key); err != nil {
		slog.Error("写周报状态失败", "err", err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("周报取用户失败", "err", err)
		return
	}
	directive := fmt.Sprintf(
		"[系统定时触发·每周汇总]（此输入来自系统调度器，不是用户本人）今天是 %s 周一，请给老板写一份公司周报。"+
			"先用 company_overview 核实全局数据，需要细节时再用 view_project、get_user_stats 等工具追查；一切以工具返回为准，不得编造。"+
			"结构：一、整体进展；二、上周完成亮点；三、风险与过期任务（点名到人、给出建议动作）；四、本周建议关注。"+
			"语言简洁，直接给结论。你的回复会作为周报直接推送给老板。",
		local.Format("2006-01-02"))
	for _, u := range users {
		if !u.IsSuperadmin || u.Status != store.UserActive {
			continue
		}
		s.dispatchAI(ctx, u, directive, "📈 每周汇总\n", "周报", nil)
	}
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
			if s.send(ctx, t.AssigneeID,
				fmt.Sprintf("⏳ 任务「%s」（#%d）将于 %s 截止，请安排进度。", t.Title, t.ID, s.fmtTime(*t.Deadline))) {
				if err := s.store.MarkDeadlineReminderSent(ctx, t.ID, time.Now().UTC()); err != nil {
					slog.Warn("临近截止提醒 ack 失败", "task", t.ID, "err", err)
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
			ok := s.send(ctx, t.AssigneeID,
				fmt.Sprintf("🔴 任务「%s」（#%d）已过截止时间（%s）。请立即更新进度，或与分配者沟通调整。", t.Title, t.ID, s.fmtTime(*t.Deadline)))
			if t.AssignerID != t.AssigneeID {
				// 分配者侧走事件总线：AI 结合 worker 在线/任务状态决定是否改派或额外通知，
				// 而非死板模板。bus 未装配（测试）时回退原文模板，保证必达。
				detail := fmt.Sprintf("你分配的任务「%s」（#%d，执行人 %s）已过截止时间（%s）。",
					t.Title, t.ID, s.userName(ctx, t.AssigneeID), s.fmtTime(*t.Deadline))
				if s.bus != nil {
					s.bus.Emit("任务过期", t.AssignerID, detail)
				} else if !s.send(ctx, t.AssignerID, "🔴 "+detail) {
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

// maybeDailySummary 每天在配置小时给每个有待办的用户推送任务清单；
// 超管额外收到全局日报（老板视角：不追问也能掌握全局）。
// 用 kv_state 记录最后发送日，重启不重发。
func (s *Scheduler) maybeDailySummary(ctx context.Context) {
	if s.dailyHour < 0 {
		return
	}
	local := time.Now().In(s.tz)
	if local.Hour() != s.dailyHour {
		return
	}
	today := local.Format("2006-01-02")
	last, err := s.store.GetKV(ctx, kvDailySummary)
	if err != nil {
		slog.Error("读每日汇总状态失败", "err", err)
		return
	}
	if last == today {
		return
	}
	// 先写状态再发送：宁可漏发一天，不可无限重发。
	if err := s.store.SetKV(ctx, kvDailySummary, today); err != nil {
		slog.Error("写每日汇总状态失败", "err", err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("每日汇总取用户失败", "err", err)
		return
	}
	digest := s.buildDigest(ctx, users)
	// 逐人查待办 + 推送：受 sendPool 限并发异步扇出，几百上千人也不会串行拖成
	// 一长串，也不阻塞调度节拍（都是廉价 DB 查询 + 一条推送）。
	for _, u := range users {
		if u.Status != store.UserActive {
			continue
		}
		u := u
		s.sendPool.submit(ctx, func() {
			tasks, err := s.store.TasksOfAssignee(ctx, u.ID, true)
			if err != nil {
				slog.Warn("每日汇总取任务失败", "user", u.ID, "err", err)
				return
			}
			if len(tasks) > 0 {
				s.send(ctx, u.ID, renderTodos(tasks, s.tz))
			}
			if u.IsSuperadmin && digest != "" {
				s.send(ctx, u.ID, digest)
			}
		})
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

func (s *Scheduler) send(ctx context.Context, userID int64, text string) bool {
	if err := s.notifier.Send(ctx, userID, text); err != nil {
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
