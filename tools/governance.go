package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
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

		tool("list_decision_queue", "查看当前用户未处理的决策队列。老板问“我现在要处理什么/今天要拍板什么”时调用。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				items, err := d.Store.ListDecisionItems(ctx, u.ID, "open", 30)
				if err != nil {
					return "", err
				}
				return renderDecisionItems(items), nil
			}),

		tool("score_learning_candidates", "对待审核学习候选做一次治理评分：识别明显重复、冲突、低证据候选，帮助超管审核。", obj(map[string]any{
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
					fmt.Fprintf(&b, "- v%d：%s（%s，%s）\n", v.Version, v.Title, v.Kind, fmtTime(v.CreatedAt, d.TZ))
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

		tool("bind_telegram_group_project", "把 Telegram 群绑定到 nbco 项目。绑定后群监控/上下文可按项目归档和总结。需要 manage_telegram_group 权限。", obj(map[string]any{
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

func clipRunes(s string, max int) string {
	rs := []rune(strings.TrimSpace(s))
	if max <= 0 || len(rs) <= max {
		return string(rs)
	}
	return string(rs[:max]) + "..."
}
