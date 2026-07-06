package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

type approvalTurnKey struct{}

type approvalTurn struct {
	SessionID int64
	MessageID int64
}

// WithApprovalTurn 标记本轮对话对应的用户消息。高危工具只能在后续用户消息
// 明确确认之后执行，避免模型在同一个 tool loop 内登记后立刻自我核销。
func WithApprovalTurn(ctx context.Context, sessionID, messageID int64) context.Context {
	if sessionID <= 0 || messageID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, approvalTurnKey{}, approvalTurn{SessionID: sessionID, MessageID: messageID})
}

func approvalTurnFromContext(ctx context.Context) (approvalTurn, bool) {
	t, ok := ctx.Value(approvalTurnKey{}).(approvalTurn)
	return t, ok && t.SessionID > 0 && t.MessageID > 0
}

// approvalRequired 两段式确认的破坏性工具清单：第一次调用只登记待确认动作，
// AI 必须向用户复述并获得明确同意后，以完全相同参数再次调用才真正执行。
// 防两类事故：模型单轮冲动执行、提示注入一击即中（注入者拿不到第二轮确认）。
// 可逆或有目标级权限校验兜底的操作不进清单——确认步骤有对话成本，只留给不可逆的。
var approvalRequired = map[string]bool{
	"disable_user":      true, // 停用账号
	"revoke_worker":     true, // 吊销 AI 员工 token
	"delete_project":    true, // 连带删除项目全部任务
	"delete_role":       true,
	"remove_info_field": true, // 连带删除全员该字段数据
}

// withApproval 给破坏性工具包上两段式确认（在审计层内侧，两次调用都留审计）。
func withApproval(s *store.Store, userID int64, t ai.Tool) ai.Tool {
	if !approvalRequired[t.Name] {
		return t
	}
	inner := t.Handler
	name := t.Name
	t.Description += "【高危：首次调用仅登记待确认动作；须向用户复述操作并获明确同意后，以完全相同参数再次调用才执行】"
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		turn, ok := approvalTurnFromContext(ctx)
		if !ok {
			return "高危操作需要在 nbco 对话里由用户下一条消息明确确认后执行；当前入口没有可验证的用户确认轮次。", nil
		}
		hash := canonicalArgsHash(args)
		ok, err := s.ConsumePendingApproval(ctx, userID, name, hash, turn.SessionID, turn.MessageID)
		if err != nil {
			return "", err
		}
		if ok {
			return inner(ctx, args)
		}
		id, err := s.CreatePendingApproval(ctx, userID, name, hash, turn.SessionID, turn.MessageID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("⚠️ 高危操作已登记为待确认动作 #%d（10 分钟内有效）。"+
			"请向用户复述将要执行的具体操作并征得明确同意；只有收到用户下一条明确确认消息后，才能以完全相同的参数再次调用本工具执行。"+
			"同一轮里不要再次调用；用户不同意或未回应就不要再调用。", id), nil
	}
	return t
}

// canonicalArgsHash 参数规范化哈希：经 map 反序列化再序列化（键有序），
// 模型两次调用只要语义参数一致，键顺序/空白差异不影响匹配。
func canonicalArgsHash(raw json.RawMessage) string {
	var v any
	if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
		if canon, err := json.Marshal(v); err == nil {
			raw = canon
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
