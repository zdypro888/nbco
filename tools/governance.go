package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

func governanceTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("refresh_decision_queue", "刷新并查看当前用户的老板/负责人决策队列：待验收、过期任务、需要拍板的阻塞。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				n, err := d.Store.BuildDecisionQueue(ctx, u.ID)
				if err != nil {
					return "", err
				}
				items, err := d.Store.ListDecisionItems(ctx, u.ID, "open", 30)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已刷新决策队列，新增/更新 %d 条。\n%s", n, renderDecisionItems(items)), nil
			}),

		tool("list_decision_queue", "查看当前用户未处理的决策队列，包含待验收、过期任务和需要拍板的阻塞。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				items, err := d.Store.ListDecisionItems(ctx, u.ID, "open", 30)
				if err != nil {
					return "", err
				}
				return renderDecisionItems(items), nil
			}),

		tool("list_action_turns", "查看最近 AI 动作轮次的意图、路由、可见工具和执行轨迹，用于诊断为什么没调用工具或某轮如何决策；它不是业务状态来源。普通用户只看自己，超管可 scope=all。", obj(map[string]any{
			"scope": p("string", "self 或 all；all 仅超级管理员有效，默认 self"),
			"limit": p("integer", "返回条数，默认20，最多80"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil {
					return "动作账本不可用。", nil
				}
				var args struct {
					Scope string `json:"scope"`
					Limit int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				limit := args.Limit
				if limit <= 0 {
					limit = 20
				}
				if limit > 80 {
					limit = 80
				}
				userID := u.ID
				if u != nil && u.IsSuperadmin && strings.EqualFold(strings.TrimSpace(args.Scope), "all") {
					userID = 0
				}
				items, err := d.Store.ListActionTurns(ctx, userID, limit)
				if err != nil {
					return "", err
				}
				return renderActionTurns(ctx, d.Store, d.TZ, items), nil
			}),

		tool("list_system_activity", "查询系统真实工具调用流水，用来核实谁在什么时间做过什么、某项变更是否发生、最近是否有人执行或更新。它直接读取通用审计账本，不依赖任务、计划或专项活动是否创建；领域状态为空时不能代替本工具证明事情没发生。不确定工具名时留空筛选并查看最近记录。仅超级管理员可用。", obj(map[string]any{
			"user_id":     p("integer", "可选：按员工内部 ID 精确筛选；0 表示所有人"),
			"session_id":  p("integer", "可选：按会话内部 ID 精确筛选"),
			"tool":        p("string", "可选：按工具名精确筛选，例如 update_my_profile"),
			"query":       p("string", "可选：在工具名、参数、结果和员工名中进行文字检索"),
			"since_hours": p("integer", "向前查询小时数，默认 24，最大 8760"),
			"limit":       p("integer", "返回条数，默认 50，最大 200"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil {
					return "系统活动账本不可用。", nil
				}
				var args struct {
					UserID     int64  `json:"user_id"`
					SessionID  int64  `json:"session_id"`
					Tool       string `json:"tool"`
					Query      string `json:"query"`
					SinceHours int    `json:"since_hours"`
					Limit      int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				hours := args.SinceHours
				if hours <= 0 {
					hours = 24
				}
				if hours > 24*365 {
					hours = 24 * 365
				}
				limit := args.Limit
				if limit <= 0 {
					limit = 50
				}
				if limit > 200 {
					limit = 200
				}
				since := time.Now().Add(-time.Duration(hours) * time.Hour)
				items, err := d.Store.ListAuditActivity(ctx, store.AuditActivityFilter{
					UserID: args.UserID, SessionID: args.SessionID, Tool: args.Tool,
					Query: args.Query, Since: &since, Limit: limit,
				})
				if err != nil {
					return "", err
				}
				return renderSystemActivity(d.TZ, items), nil
			}),

		tool("score_learning_candidates", "对待审核学习候选做一次确定性预评分：识别精确重复并标出文本相关候选；语义重复或冲突仍由 Agent 结合检索结果审核。", obj(map[string]any{
			"limit": p("integer", "最多处理多少条，默认200"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Limit int `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				n, err := d.Store.ScoreLearningCandidates(ctx, args.Limit)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已评分 %d 条学习候选。请用 list_learning_candidates 查看 value_score/review_note。", n), nil
			}),

		tool("list_knowledge_versions", "查看知识/规则/skill 的历史版本。用于确认某条记忆如何变化，以及回滚前核对。", obj(map[string]any{
			"knowledge_id": p("integer", "知识/规则/skill ID"),
			"limit":        p("integer", "返回条数，默认20"),
		}, "knowledge_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					KnowledgeID int64 `json:"knowledge_id"`
					Limit       int   `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				vs, err := d.Store.KnowledgeVersions(ctx, args.KnowledgeID, args.Limit)
				if err != nil {
					return "", err
				}
				if len(vs) == 0 {
					return "暂无历史版本。", nil
				}
				var b strings.Builder
				for _, v := range vs {
					state := "生效"
					if !v.Active {
						state = "已归档"
					}
					fmt.Fprintf(&b, "- v%d：%s（%s，%s，%s）\n", v.Version, v.Title, v.Kind, state, fmtTime(v.CreatedAt, d.TZ))
					if strings.TrimSpace(v.ChangeNote) != "" {
						fmt.Fprintf(&b, "  note: %s\n", v.ChangeNote)
					}
				}
				return strings.TrimSpace(b.String()), nil
			}),

		tool("rollback_knowledge", "把一条知识/规则/skill 回滚到指定历史版本。用于学习错误、规则误改后的恢复。", obj(map[string]any{
			"knowledge_id": p("integer", "知识/规则/skill ID"),
			"version":      p("integer", "要回滚到的版本号"),
		}, "knowledge_id", "version"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					KnowledgeID int64 `json:"knowledge_id"`
					Version     int   `json:"version"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				k, err := d.Store.RollbackKnowledge(ctx, args.KnowledgeID, args.Version, u.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "知识或版本不存在。", nil
					}
					return "", err
				}
				if d.Knowledge != nil {
					d.Knowledge.Reembed(ctx, k)
				}
				return fmt.Sprintf("已回滚 %s 到 v%d：%s", internalRef("知识", k.ID), args.Version, k.Title), nil
			}),

		tool("list_material_entities", "查看从公司资料里提取出的结构化实体/事实（客户、项目、合同、制度、联系人等）。", obj(map[string]any{
			"entity_type": p("string", "实体类型，可选，如 customer/project/contract/policy/contact"),
			"limit":       p("integer", "默认50"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					EntityType string `json:"entity_type"`
					Limit      int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				items, err := d.Store.ListMaterialEntities(ctx, args.EntityType, args.Limit)
				if err != nil {
					return "", err
				}
				if len(items) == 0 {
					return "（暂无材料实体）", nil
				}
				var b strings.Builder
				for _, e := range items {
					fmt.Fprintf(&b, "- %s [%s] %s：%s\n", internalRef("实体", e.ID), e.EntityType, e.Name, clipRunes(e.Content, 160))
				}
				return strings.TrimSpace(b.String()), nil
			}),

		tool("create_org_group", "创建组织组/项目组/部门。用于把权限、项目群、负责人和员工关系结构化。", obj(map[string]any{
			"name":        p("string", "组名"),
			"description": p("string", "说明，可选"),
			"manager_id":  p("integer", "负责人用户ID，可选"),
		}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					ManagerID   int64  `json:"manager_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				name := strings.TrimSpace(args.Name)
				if name == "" {
					return "组名不能为空。", nil
				}
				var manager *int64
				if args.ManagerID > 0 {
					if _, err := mustUser(ctx, d.Store, args.ManagerID); err != nil {
						return err.Error(), nil
					}
					manager = &args.ManagerID
				}
				by := u.ID
				g, err := d.Store.CreateOrgGroup(ctx, name, strings.TrimSpace(args.Description), manager, &by)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已创建组织组「%s」（%s）。", g.Name, internalRef("组", g.ID)), nil
			}),

		tool("list_org_groups", "列出组织组/项目组/部门。", obj(map[string]any{
			"limit": p("integer", "默认100"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Limit int `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				groups, err := d.Store.ListOrgGroups(ctx, args.Limit)
				if err != nil {
					return "", err
				}
				if len(groups) == 0 {
					return "（暂无组织组）", nil
				}
				var b strings.Builder
				for _, g := range groups {
					manager := ""
					if g.ManagerID != nil {
						manager = "，负责人 " + userName(ctx, d.Store, *g.ManagerID)
					}
					fmt.Fprintf(&b, "- %s：%s%s\n", internalRef("组", g.ID), g.Name, manager)
				}
				return strings.TrimSpace(b.String()), nil
			}),

		tool("add_org_group_member", "把用户加入组织组/项目组，并记录其组内角色。", obj(map[string]any{
			"group_id": p("integer", "组织组ID"),
			"user_id":  p("integer", "用户ID"),
			"role":     p("string", "member/lead/observer 等，可选"),
		}, "group_id", "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					GroupID int64  `json:"group_id"`
					UserID  int64  `json:"user_id"`
					Role    string `json:"role"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, err := mustUser(ctx, d.Store, args.UserID); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.AddOrgGroupMember(ctx, args.GroupID, args.UserID, strings.TrimSpace(args.Role)); err != nil {
					return "", err
				}
				return "已加入组织组。", nil
			}),

		tool("bind_telegram_group_project", "把 Telegram 群绑定到系统项目。绑定后群监控/上下文可按项目归档和总结。需要 manage_telegram_group 权限。", obj(map[string]any{
			"group":      p("string", "群名、群名片段或 group_ref"),
			"project_id": p("integer", "项目ID"),
			"note":       p("string", "说明，可选"),
		}, "group", "project_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group     string `json:"group"`
					ProjectID int64  `json:"project_id"`
					Note      string `json:"note"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
				if err != nil {
					return fmt.Sprintf("项目 %d 不存在。", args.ProjectID), nil
				}
				if err := d.Store.BindTelegramGroupProject(ctx, g.ChatID, pj.ID, u.ID, strings.TrimSpace(args.Note)); err != nil {
					return "", err
				}
				return fmt.Sprintf("已把 Telegram 群「%s」绑定到项目「%s」。", telegramGroupTitle(*g), pj.Name), nil
			}),

		tool("list_eval_cases", "列出对话回归测试用例。用于检查 Telegram 格式、工具调用纪律、隐私输出等质量红线。", obj(map[string]any{
			"enabled_only": p("boolean", "只看启用用例，默认 true"),
			"limit":        p("integer", "默认50"),
		}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					EnabledOnly bool `json:"enabled_only"`
					Limit       int  `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				items, err := d.Store.ListConversationEvalCases(ctx, args.EnabledOnly || args.Limit == 0, args.Limit)
				if err != nil {
					return "", err
				}
				if len(items) == 0 {
					return "（暂无 eval case）", nil
				}
				var b strings.Builder
				for _, c := range items {
					state := "disabled"
					if c.Enabled {
						state = "enabled"
					}
					fmt.Fprintf(&b, "- %s：%s [%s/%s]\n", internalRef("eval", c.ID), c.Name, c.Channel, state)
				}
				return strings.TrimSpace(b.String()), nil
			}),

		tool("create_eval_case", "创建对话回归测试用例。assertions 是 JSON 对象，如 forbidden_substrings/required_substrings/channel/no_markdown。", obj(map[string]any{
			"name":       p("string", "用例名"),
			"channel":    p("string", "telegram/api，默认 telegram"),
			"user_input": p("string", "输入"),
			"assertions": map[string]any{"type": "object", "description": "断言 JSON"},
		}, "name", "user_input"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name       string         `json:"name"`
					Channel    string         `json:"channel"`
					UserInput  string         `json:"user_input"`
					Assertions map[string]any `json:"assertions"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				assertions, _ := json.Marshal(args.Assertions)
				by := u.ID
				c, err := d.Store.CreateConversationEvalCase(ctx, store.ConversationEvalCase{
					Name: strings.TrimSpace(args.Name), Channel: strings.TrimSpace(args.Channel),
					UserInput: strings.TrimSpace(args.UserInput), Assertions: assertions,
					Enabled: true, CreatedBy: &by,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已创建 eval case（%s）：%s。", internalRef("eval", c.ID), c.Name), nil
			}),
	}
}

func renderDecisionItems(items []*store.DecisionItem) string {
	if len(items) == 0 {
		return "（当前没有待决策事项）"
	}
	var b strings.Builder
	for _, it := range items {
		ref := ""
		if it.RefID != nil && it.RefType != "" {
			ref = fmt.Sprintf("（%s #%d）", it.RefType, *it.RefID)
		}
		fmt.Fprintf(&b, "- %s [%s] %s%s\n", internalRef("决策", it.ID), it.Priority, it.Title, ref)
		if strings.TrimSpace(it.Detail) != "" {
			fmt.Fprintf(&b, "  %s\n", it.Detail)
		}
	}
	return strings.TrimSpace(b.String())
}

func renderSystemActivity(tz *time.Location, items []*store.AuditActivity) string {
	if len(items) == 0 {
		return "没有匹配的系统活动记录。这个结果只覆盖当前筛选条件和时间范围，不能证明其他操作或更早时间从未发生。"
	}
	if tz == nil {
		tz = time.Local
	}
	var b strings.Builder
	b.WriteString("系统工具调用流水（handler 状态只表示工具处理器是否报系统错误，业务结果以结果正文为准）\n")
	for _, item := range items {
		if item == nil {
			continue
		}
		name := strings.TrimSpace(item.UserName)
		if name == "" {
			name = fmt.Sprintf("员工ID %d", item.UserID)
		}
		state := "handler-ok"
		if !item.OK {
			state = "system-error"
		}
		fmt.Fprintf(&b, "- 活动内部编号 %d · %s · %s（员工ID %d）· %s [%s]\n",
			item.ID, item.CreatedAt.In(tz).Format("2006-01-02 15:04:05"), name, item.UserID, item.Tool, state)
		args := strings.TrimSpace(string(item.Args))
		if args != "" && args != "{}" && args != "null" {
			fmt.Fprintf(&b, "  参数：%s\n", clipRunes(textfmt.RedactSecrets(args), 360))
		}
		if result := strings.TrimSpace(item.Result); result != "" {
			fmt.Fprintf(&b, "  结果：%s\n", clipRunes(textfmt.RedactSecrets(result), 500))
		}
	}
	return strings.TrimSpace(b.String())
}

type actionTurnDetails struct {
	FinishReason string `json:"finish_reason"`
	TurnContext  struct {
		Route                string   `json:"route"`
		SystemChars          int      `json:"system_chars"`
		HistoryChars         int      `json:"history_chars"`
		ToolCount            int      `json:"tool_count"`
		FullToolCount        int      `json:"full_tool_count"`
		ToolSchemaChars      int      `json:"tool_schema_chars"`
		Tools                []string `json:"tools"`
		AgentIterations      int      `json:"agent_iterations"`
		ModelCalls           int      `json:"model_calls"`
		ModelPeakToolCount   int      `json:"model_peak_tool_count"`
		ModelPeakSchemaChars int      `json:"model_peak_schema_chars"`
		ModelPeakTools       []string `json:"model_peak_tools"`
	} `json:"turn_context"`
	ToolEvidence []struct {
		Tool            string `json:"tool"`
		HandlerReturned bool   `json:"handler_returned"`
		LegacyOK        bool   `json:"ok"`
		Summary         string `json:"summary"`
	} `json:"tool_evidence"`
}

func renderActionTurns(ctx context.Context, s *store.Store, tz *time.Location, items []*store.ActionTurn) string {
	if len(items) == 0 {
		return "（暂无动作轮次记录）"
	}
	if tz == nil {
		tz = time.Local
	}
	var b strings.Builder
	b.WriteString("最近动作轮次\n")
	for _, it := range items {
		if it == nil {
			continue
		}
		status := actionOutcomeLabel(it.Outcome)
		name := userName(ctx, s, it.UserID)
		fmt.Fprintf(&b, "- #%d %s %s [%s] %s；handler 返回 %d/%d",
			it.ID, it.CreatedAt.In(tz).Format("01-02 15:04"), name, it.Channel, status, it.SuccessToolCount, it.ToolCount)
		if strings.TrimSpace(it.Intent) != "" {
			fmt.Fprintf(&b, "；意图：%s", clipRunes(it.Intent, 80))
		}
		b.WriteByte('\n')
		if strings.TrimSpace(it.UserTextExcerpt) != "" {
			fmt.Fprintf(&b, "  输入：%s\n", clipRunes(it.UserTextExcerpt, 140))
		}
		if strings.TrimSpace(it.ReplyExcerpt) != "" {
			fmt.Fprintf(&b, "  回复：%s\n", clipRunes(it.ReplyExcerpt, 140))
		}
		var details actionTurnDetails
		if len(it.Evidence) > 0 {
			_ = json.Unmarshal(it.Evidence, &details)
		}
		if len(it.ExpectedTools) > 0 {
			fmt.Fprintf(&b, "  实际动作工具：%s\n", strings.Join(it.ExpectedTools, ", "))
		}
		if len(details.ToolEvidence) > 0 {
			b.WriteString("  工具证据：\n")
			for _, ev := range details.ToolEvidence {
				state := "handler_error"
				if ev.HandlerReturned || ev.LegacyOK {
					state = "returned"
				}
				fmt.Fprintf(&b, "  - %s:%s %s\n", ev.Tool, state, clipRunes(ev.Summary, 180))
			}
		}
		if details.TurnContext.ToolCount > 0 || details.TurnContext.SystemChars > 0 {
			fmt.Fprintf(&b, "  上下文：route=%s catalog=%d/%d catalog_schema_chars=%d system_chars=%d history_chars=%d",
				details.TurnContext.Route, details.TurnContext.ToolCount, details.TurnContext.FullToolCount,
				details.TurnContext.ToolSchemaChars, details.TurnContext.SystemChars, details.TurnContext.HistoryChars)
			if details.TurnContext.ModelCalls > 0 {
				fmt.Fprintf(&b, " model_peak=%d/%d agent_iterations=%d model_calls=%d",
					details.TurnContext.ModelPeakToolCount, details.TurnContext.ModelPeakSchemaChars,
					details.TurnContext.AgentIterations, details.TurnContext.ModelCalls)
			}
			b.WriteByte('\n')
		}
		if details.FinishReason != "" {
			fmt.Fprintf(&b, "  finish_reason: %s\n", details.FinishReason)
		}
	}
	return strings.TrimSpace(b.String())
}

func actionOutcomeLabel(outcome string) string {
	switch outcome {
	case "action_tool_returned":
		return "动作工具已返回（业务结果见明细）"
	case "read_tool_returned":
		return "只读工具已返回"
	case "answered_without_tool":
		return "未调用工具"
	case "tool_handler_error":
		return "工具 handler 错误"
	case "pending_approval":
		return "等待用户确认"
	case "evidence_ok":
		return "历史记录：曾判定已执行"
	case "planned_without_tool":
		return "规划了动作但没调用工具"
	case "tool_attempted_without_success_evidence":
		return "调用过工具但无成功证据"
	case "blocked_action_evidence", "blocked_no_tool_completion":
		return "历史版本曾拦截"
	case "no_result":
		return "无模型结果"
	case "no_action":
		return "非动作请求"
	default:
		if strings.TrimSpace(outcome) == "" {
			return "未知"
		}
		return outcome
	}
}

func clipRunes(s string, max int) string {
	rs := []rune(strings.TrimSpace(s))
	if max <= 0 || len(rs) <= max {
		return string(rs)
	}
	return string(rs[:max]) + "..."
}
