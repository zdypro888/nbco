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
		hash := canonicalArgsHash(args)
		ok, err := s.ConsumePendingApproval(ctx, userID, name, hash)
		if err != nil {
			return "", err
		}
		if ok {
			return inner(ctx, args)
		}
		id, err := s.CreatePendingApproval(ctx, userID, name, hash)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("⚠️ 高危操作已登记为待确认动作 #%d（10 分钟内有效）。"+
			"请向用户复述将要执行的具体操作并征得明确同意；同意后以完全相同的参数再次调用本工具即执行。"+
			"用户不同意或未回应就不要再调用。", id), nil
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
