package store

import "context"

// 事件处理结果（events 表 outcome 字段取值）。
const (
	EventOutcomeHandled    = "handled"     // AI 产出通知并投递成功
	EventOutcomeSkipped    = "skipped"     // AI 判定不值得打扰，静默
	EventOutcomeFallback   = "fallback"    // AI 不可用/失败/空答复，降级原文推送
	EventOutcomeDropped    = "dropped"     // 决策人不可达（不存在/非活跃），事件丢弃
	EventOutcomeSendFailed = "send_failed" // AI 产出但通知投递失败
)

// eventReplyMax 审计记录里 reply 摘要的最大字符数（rune）。
const eventReplyMax = 500

// RecordEvent 落一条系统事件处理记录（审计/可观测）。在事件处理完成后调用一次，
// outcome 见 EventOutcome* 常量。失败不阻断业务（调用方记日志）。
func (s *Store) RecordEvent(ctx context.Context, kind string, deciderID int64, detail, outcome, reply string) error {
	return s.execOne(ctx,
		`INSERT INTO events (kind, decider_id, detail, outcome, reply) VALUES ($1,$2,$3,$4,$5)`,
		kind, deciderID, detail, outcome, truncateRunes(reply, eventReplyMax))
}

// truncateRunes 按 rune 数截断（不破坏 UTF-8），超出加省略号。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
