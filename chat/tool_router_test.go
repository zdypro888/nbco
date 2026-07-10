package chat

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	nbtools "github.com/zdypro888/nbco/tools"
)

func TestRouteTurnToolsKeepsPlainQuestionSmall(t *testing.T) {
	routed, route := routeTurnTools("telegram", "你现在能做什么？简单说", testRouteTools())
	names := routeToolNameSet(routed)
	if !names["list_capabilities"] || !names["search_knowledge"] {
		t.Fatalf("基础查询工具应保留: %v", routedToolNames(routed))
	}
	for _, bad := range []string{"send_message", "assign_task", "start_workflow", "run_worker_command", "schedule_push"} {
		if names[bad] {
			t.Fatalf("普通问答不应暴露 %s，tools=%v route=%s", bad, routedToolNames(routed), route.Summary())
		}
	}
	if len(routed) >= len(testRouteTools()) {
		t.Fatalf("路由后工具应小于全量: got=%d full=%d", len(routed), len(testRouteTools()))
	}
}

func TestRouteTurnToolsIncludesExtensionTools(t *testing.T) {
	all := append(testRouteTools(), ai.Tool{
		Name: "weather_lookup", Description: "查询天气预报", Effect: ai.ToolEffectRead,
	})
	routed, route := routeTurnTools("telegram", "查一下明天的天气", all)
	if !routeToolNameSet(routed)["weather_lookup"] {
		t.Fatalf("extension tool was dropped: tools=%v route=%s", routedToolNames(routed), route.Summary())
	}
	if !route.Has("extension") {
		t.Fatalf("extension route missing: %s", route.Summary())
	}
}

func TestToolSoftLimitKeepsRelevantLateToolWithoutAllowlist(t *testing.T) {
	in := make([]ai.Tool, 0, 70)
	for i := range 69 {
		in = append(in, ai.Tool{Name: "generic_" + strconv.Itoa(i), Description: "普通管理能力"})
	}
	in = append(in, ai.Tool{Name: "new_milestone_editor", Description: "修改里程碑标题说明和截止时间"})
	got := keepRoutedToolsUnderSoftLimit("请修改里程碑截止时间", in, nil, nil)
	if len(got) != routedToolSoftLimit {
		t.Fatalf("软上限结果数 = %d", len(got))
	}
	if !routeToolNameSet(got)["new_milestone_editor"] {
		t.Fatalf("末尾新增的相关工具被静默裁掉: %v", routedToolNames(got))
	}
}

func TestExtensionToolCanProvideActionEvidence(t *testing.T) {
	custom := ai.Tool{Name: "company_action", Description: "执行公司扩展动作", Effect: ai.ToolEffectExecute}
	steps := []ai.Step{{Kind: ai.StepToolCall, ToolName: custom.Name, Result: "执行成功"}}
	if sideEffectCompletionWithoutSuccessfulActionWithTools("执行扩展动作", "已完成。", []ai.Tool{custom}, steps) {
		t.Fatal("successful extension execution must satisfy action evidence")
	}
	plan := buildActionAuditPlan("执行扩展动作", []ai.Tool{custom}, &ai.TurnResult{Steps: steps})
	if plan == nil || !containsString(plan.ExpectedTools, custom.Name) {
		t.Fatalf("actual extension action must be recorded in audit plan: %+v", plan)
	}
}

func TestInferActionToolsUsesEffectMetadataAndDescription(t *testing.T) {
	toolset := []ai.Tool{
		{Name: "weather_update", Description: "更新城市天气预报配置", Effect: ai.ToolEffectWrite},
		{Name: "unrelated_write", Description: "修改员工排班", Effect: ai.ToolEffectWrite},
		{Name: "weather_read", Description: "查询城市天气预报", Effect: ai.ToolEffectRead},
	}
	got := inferActionToolsForText("更新天气预报", toolset, 8)
	if len(got) != 1 || got[0] != "weather_update" {
		t.Fatalf("动作工具推导未遵循 effect/相关性元数据: %v", got)
	}
}

func TestGroupSensitiveExtensionIsStrippedByMetadata(t *testing.T) {
	tools := []ai.Tool{{Name: "custom_admin", GroupSensitive: true}, {Name: "custom_weather"}}
	got := nbtools.StripGroupSensitive(tools)
	if len(got) != 1 || got[0].Name != "custom_weather" {
		t.Fatalf("group metadata filtering failed: %+v", got)
	}
}

func TestRouteTurnToolsAddsCommsAndSchedule(t *testing.T) {
	routed, route := routeTurnTools("telegram", "明天早上9点通知全体员工完善档案", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"send_message", "schedule_push", "list_users", "create_data_collection_campaign"} {
		if !names[want] {
			t.Fatalf("通知/定时场景缺 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
	if names["run_worker_command"] {
		t.Fatalf("通知/定时场景不应默认暴露 worker 命令: %v", routedToolNames(routed))
	}

	routed, route = routeTurnTools("telegram", "给黄桑发消息说明今天开会", testRouteTools())
	names = routeToolNameSet(routed)
	if !names["send_message"] || !looksLikeAuditableActionRequest("给黄桑发消息说明今天开会") {
		t.Fatalf("发消息场景应暴露 send_message 并进入动作审计，tools=%v route=%s", routedToolNames(routed), route.Summary())
	}
}

func TestRouteTurnToolsAddsWorkerAndFiles(t *testing.T) {
	routed, route := routeTurnTools("telegram", "把刚才上传的两个 xlsx 文件交给 worker 整理成人事资料", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"list_recent_files", "analyze_company_materials", "start_workflow", "start_worker_skill", "list_workers"} {
		if !names[want] {
			t.Fatalf("文件/worker 场景缺 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
}

func TestRouteTurnToolsAddsFilesForRecentAttachmentReference(t *testing.T) {
	routed, route := routeTurnTools("telegram", "能看得懂这个吗？", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"list_recent_files", "analyze_company_materials", "start_workflow", "start_worker_skill"} {
		if !names[want] {
			t.Fatalf("最近附件指代场景缺 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
	if names["run_worker_command"] {
		t.Fatalf("读取附件不应默认暴露命令执行工具: %v", routedToolNames(routed))
	}
}

func TestRouteTurnToolsDoesNotTreatEveryDeicticAsFile(t *testing.T) {
	routed, route := routeTurnTools("telegram", "这个问题怎么处理？", testRouteTools())
	names := routeToolNameSet(routed)
	for _, bad := range []string{"analyze_company_materials", "start_workflow", "start_worker_skill"} {
		if names[bad] {
			t.Fatalf("普通指代问题不应暴露文件分析工具 %s，tools=%v route=%s", bad, routedToolNames(routed), route.Summary())
		}
	}
}

func TestRouteTurnToolsKeepsCompanyOverviewForSystemTaskStatus(t *testing.T) {
	routed, route := routeTurnTools("telegram", "系统级别任务现在有哪些？全公司是不是空闲？", testRouteTools())
	names := routeToolNameSet(routed)
	if !names["company_overview"] || !names["get_my_tasks"] {
		t.Fatalf("系统级任务查询应暴露公司全景和个人任务工具，tools=%v route=%s", routedToolNames(routed), route.Summary())
	}
}

func TestRouteTurnToolsAddsActionToolsForBareDeleteRequest(t *testing.T) {
	routed, route := routeTurnTools("telegram", "无成人陪伴这个删除掉吧。没用了", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"get_assigned_tasks", "get_task_detail", "delete_assigned_task"} {
		if !names[want] {
			t.Fatalf("裸删除动作应暴露任务定位/删除工具 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
	if !route.Has("action") {
		t.Fatalf("裸删除动作应带 action route，route=%s", route.Summary())
	}
}

func TestRouteTurnToolsAddsLowLevelOpsOnlyForExplicitFallback(t *testing.T) {
	routed, route := routeTurnTools("telegram", "实在不行用底层 SQL 查一下 tasks 里这个任务的状态", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"low_level_db_query", "low_level_db_exec", "list_action_turns"} {
		if !names[want] {
			t.Fatalf("底层兜底场景缺 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
	if !route.Has("ops") {
		t.Fatalf("底层兜底场景应带 ops route，route=%s", route.Summary())
	}

	routed, route = routeTurnTools("telegram", "无成人陪伴这个删除掉吧。没用了", testRouteTools())
	names = routeToolNameSet(routed)
	if names["low_level_db_exec"] {
		t.Fatalf("普通删除不应默认暴露底层写库工具，tools=%v route=%s", routedToolNames(routed), route.Summary())
	}
}

func TestRouteTurnToolsAddsTelegramGroupTools(t *testing.T) {
	routed, route := routeTurnTools("telegram:group:-100", "@bot 监听这个群并能撤回自己发错的消息", testRouteTools())
	names := routeToolNameSet(routed)
	for _, want := range []string{"list_telegram_groups", "set_telegram_group_listen", "set_telegram_group_monitor", "delete_telegram_group_message"} {
		if !names[want] {
			t.Fatalf("群场景缺 %s，tools=%v route=%s", want, routedToolNames(routed), route.Summary())
		}
	}
}

func TestAuditableActionRequestOnlyForActionLikeTurns(t *testing.T) {
	if looksLikeAuditableActionRequest("解释一下 token 为什么不能查询明文") {
		t.Fatal("普通解释问题不应进入动作审计")
	}
	for _, text := range []string{"clone了吗？", "刚才通知发出去没？", "部署成功了吗？", "有没有执行 worker 命令？"} {
		if looksLikeAuditableActionRequest(text) {
			t.Fatalf("状态核实问题不应被记录成新动作请求: %s", text)
		}
	}
	for _, text := range []string{
		"明天早上 9 点提醒全体员工完善档案",
		"把 worker 重命名为 NBAI",
		"把黄桑的手机号改成 13800000000",
		"分析刚才上传的 pdf 并整理成公司资料",
		"给 nbco 增加天气功能然后部署",
		"帮我部署一下？",
		"无成人陪伴这个删除掉吧。没用了",
	} {
		if !looksLikeAuditableActionRequest(text) {
			t.Fatalf("操作/worker/文件请求应进入动作审计: %s", text)
		}
	}
}

func TestSlimSystemPromptAvoidsStaticDispatchTrees(t *testing.T) {
	o := &Orchestrator{tz: time.UTC}
	u := &store.User{ID: 1, Name: "PRO", IsSuperadmin: true}
	got, err := o.systemPrompt(context.Background(), u, "telegram",
		map[string]bool{"list_capabilities": true, "send_message": true, "list_roles": true},
		toolRoute{Reasons: []string{"people", "action"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"核心原则", "时间结论以当前业务时区", "本轮能力路由", "send_message", "当前用户：PRO"} {
		if !strings.Contains(got, want) {
			t.Fatalf("短系统提示缺 %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"长期/战略性目标", "运营节奏（上下班时间", "代码修改、系统运维、部署升级", "可用角色（当前工作场景匹配时"} {
		if strings.Contains(got, bad) {
			t.Fatalf("系统提示不应再常驻旧长提示 %q:\n%s", bad, got)
		}
	}
}

func TestRouteCapabilityPromptStaysCompact(t *testing.T) {
	p := routeCapabilityPrompt(map[string]bool{
		"send_message":       true,
		"schedule_push":      true,
		"start_worker_skill": true,
		"save_rule":          true,
	})
	for _, want := range []string{"send_message", "schedule", "worker", "rule"} {
		if !strings.Contains(p, want) {
			t.Fatalf("能力提示缺 %q: %s", want, p)
		}
	}
	if strings.Contains(p, "长期/战略性目标") || strings.Contains(p, "运营节奏") {
		t.Fatalf("短能力提示不应退回旧长决策树: %s", p)
	}
}

func testRouteTools() []ai.Tool {
	names := append([]string{}, baselineToolNames...)
	for _, group := range [][]string{
		peopleToolNames, permissionToolNames, workToolNames, scheduleToolNames, workerToolNames,
		fileToolNames, telegramGroupToolNames, memoryToolNames, opsToolNames, scriptToolNames,
	} {
		names = append(names, group...)
	}
	seen := map[string]bool{}
	var out []ai.Tool
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		n := name
		out = append(out, ai.Tool{
			Name:        n,
			Description: "test tool " + n,
			InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return "ok", nil
			},
		})
	}
	return out
}

func routeToolNameSet(ts []ai.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[t.Name] = true
	}
	return out
}
