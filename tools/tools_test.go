package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	jsonschema "github.com/eino-contrib/jsonschema"
	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

// TestAllToolSchemasValid 全部内建工具的 InputSchema 必须是合法 object schema：
// properties 序列化后必须是对象而非 null（null 会让 claude CLI 拒掉整个 tools/list）。
func TestAllToolSchemasValid(t *testing.T) {
	super := &store.User{ID: 1, Name: "t", Status: store.UserActive, IsSuperadmin: true}
	for _, tool := range ForUser(Deps{}, super, nil) {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: schema 序列化失败: %v", tool.Name, err)
		}
		var schema struct {
			Type       string          `json:"type"`
			Properties json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema 反序列化失败: %v", tool.Name, err)
		}
		if schema.Type != "object" {
			t.Errorf("%s: type = %q, 应为 object", tool.Name, schema.Type)
		}
		if len(schema.Properties) == 0 || schema.Properties[0] != '{' {
			t.Errorf("%s: properties 必须是对象, got %s", tool.Name, schema.Properties)
		}
		var einoSchema jsonschema.Schema
		if err := json.Unmarshal(raw, &einoSchema); err != nil {
			t.Errorf("%s: Eino JSON Schema 解析失败: %v", tool.Name, err)
		}
	}
}

func TestParseTarget(t *testing.T) {
	if key, _, isAll, err := parseTarget(store.TargetAll); err != nil || !isAll || key != store.TargetAll {
		t.Errorf("_all 解析结果 = (%q, all=%v, err=%v)", key, isAll, err)
	}
	if key, id, isAll, err := parseTarget(" 42 "); err != nil || isAll || key != "42" || id != 42 {
		t.Errorf("带空白数字解析结果 = (%q, %d, all=%v, err=%v)", key, id, isAll, err)
	}
	if _, _, _, err := parseTarget("abc"); err == nil {
		t.Error("非数字非 _all 应报错")
	}
	if _, _, _, err := parseTarget(""); err == nil {
		t.Error("空串应报错")
	}
}

func TestDecode(t *testing.T) {
	type args struct {
		Name string `json:"name"`
	}
	var v args
	if err := decode(nil, &v); err != nil || v.Name != "" {
		t.Errorf("空参数应得到零值: %+v, err=%v", v, err)
	}
	if err := decode([]byte(`{"name":"x"}`), &v); err != nil || v.Name != "x" {
		t.Errorf("正常解析: %+v, err=%v", v, err)
	}
	if err := decode([]byte(`{bad`), &v); err == nil || !strings.Contains(err.Error(), "参数格式错误") {
		t.Errorf("坏 JSON 应返回可回给模型的提示, got %v", err)
	}
}

func TestMissingInfoFieldNamesUseCanonicalDefaults(t *testing.T) {
	got := missingInfoFieldNames(map[string]*string{
		"手机号": nil,
		"部门":  nil,
		"爱好":  nil,
	}, []string{"手机"})
	joined := strings.Join(got, ",")
	if joined != "爱好,组别" {
		t.Fatalf("missing fields = %q", joined)
	}
	campaign := strings.Join(canonicalDataFields([]string{"手机号", "部门", "phone", "职位"}), ",")
	if campaign != "手机,组别,职位" {
		t.Fatalf("campaign fields = %q", campaign)
	}
}

func TestDataCampaignTargetNameFallback(t *testing.T) {
	if got := dataCampaignTargetName(store.DataCollectionCampaignTarget{UserID: 7, UserName: "黄桑"}); got != "黄桑" {
		t.Fatalf("target name = %q", got)
	}
	if got := dataCampaignTargetName(store.DataCollectionCampaignTarget{UserID: 7}); got != "员工ID 7" {
		t.Fatalf("target fallback = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("短串不应截断: %q", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("恰好等长不应截断: %q", got)
	}
	if got := truncate("hello!", 5); got != "hello…" {
		t.Errorf("超长应截断加省略号: %q", got)
	}
	if got := truncate("你好世界", 5); !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("截断不能切坏 UTF-8: %q", got)
	}
}

func TestTruncateToolOutput(t *testing.T) {
	// 短输出不截断。
	short := strings.Repeat("a", 100)
	if got := truncateToolOutput(short); got != short {
		t.Errorf("短输出不应截断, got len=%d", len(got))
	}
	// 超限截断 + 附分页提示。
	big := strings.Repeat("a", toolOutputLimit+5000)
	got := truncateToolOutput(big)
	if !strings.HasPrefix(got, strings.Repeat("a", toolOutputLimit)) {
		t.Errorf("应截断到 %d rune", toolOutputLimit)
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("截断应附分页提示, got 末尾: %q", got[len(got)-40:])
	}
	// 多字节安全：中文不切坏。
	cn := strings.Repeat("你", toolOutputLimit+100)
	if got := truncateToolOutput(cn); !utf8.ValidString(got) {
		t.Errorf("截断中文不能切坏 UTF-8")
	}
}

func TestWithAuditWithoutStore(t *testing.T) {
	tl := withAudit(nil, 1, nil, ai.Tool{
		Name: "big",
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", toolOutputLimit+1), nil
		},
	})
	got, err := tl.Handler(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "已截断") {
		t.Fatalf("无 Store 审计路径仍应执行并截断输出: len=%d", len([]rune(got)))
	}
}

func TestBoundedAuditArgsRedactsAndSummarizes(t *testing.T) {
	small := boundedAuditArgs(json.RawMessage(`{"token":"sk-test-0123456789abcdef0123456789abcdef","value":"ok"}`))
	if !json.Valid(small) || strings.Contains(string(small), "0123456789abcdef") || !strings.Contains(strings.ToLower(string(small)), "[redacted]") {
		t.Fatalf("small audit args were not safely redacted: %s", small)
	}

	large := boundedAuditArgs(json.RawMessage(`{"content":"` + strings.Repeat("x", auditArgsLimit) + `"}`))
	var envelope struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(large, &envelope); err != nil || !envelope.Truncated ||
		envelope.Bytes <= auditArgsLimit || len(envelope.SHA256) != 64 || len(envelope.Preview) == 0 || len(large) >= auditArgsLimit {
		t.Fatalf("large audit args were not summarized: %s, err=%v", large, err)
	}
}

func TestCapabilityRegistryMetadata(t *testing.T) {
	super := &store.User{ID: 1, Name: "boss", Status: store.UserActive, IsSuperadmin: true}
	caps, err := CapabilityRegistry(context.Background(), Deps{}, super, true)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Capability{}
	for _, c := range caps {
		byName[c.Name] = c
	}
	for _, name := range []string{"assign_task", "analyze_company_materials", "delegate_worker_agent", "start_worker_skill", "start_workflow", "create_data_collection_campaign", "schedule_once_push", "schedule_recurring_push", "list_telegram_group_messages", "set_telegram_group_digest", "list_capabilities", "query_data", "list_action_turns", "list_system_activity", "low_level_db_query", "low_level_db_exec"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("能力目录缺 %s", name)
		}
	}
	if got := byName["assign_task"].Domain; got != CapabilityWork {
		t.Fatalf("assign_task domain=%q", got)
	}
	if got := byName["analyze_company_materials"].Domain; got != CapabilityWorkers {
		t.Fatalf("analyze_company_materials domain=%q", got)
	}
	if got := byName["start_worker_skill"].Domain; got != CapabilityWorkers {
		t.Fatalf("start_worker_skill domain=%q", got)
	}
	if got := byName["start_workflow"].RequiredAction; got != "manage_worker" {
		t.Fatalf("start_workflow required_action=%q", got)
	}
	if got := byName["list_action_turns"].Domain; got != CapabilityOps {
		t.Fatalf("list_action_turns domain=%q", got)
	}
	if got := byName["list_action_turns"].Effect; got != ToolEffectRead {
		t.Fatalf("list_action_turns effect=%q", got)
	}
	if got := byName["list_system_activity"].Effect; got != ToolEffectRead {
		t.Fatalf("list_system_activity effect=%q", got)
	}
	if !byName["list_system_activity"].SuperadminOnly || byName["list_system_activity"].GroupAllowed {
		t.Fatalf("list_system_activity 应仅超管可用且禁止群聊: %+v", byName["list_system_activity"])
	}
	if got := byName["query_data"]; got.Domain != CapabilityOps || got.Effect != ToolEffectRead || got.GroupAllowed || !got.WorkerAllowed || got.LoadMode != string(ai.ToolLoadImmediate) {
		t.Fatalf("query_data 元数据错误: %+v", got)
	}
	if got := byName["list_capabilities"]; got.LoadMode != string(ai.ToolLoadImmediate) {
		t.Fatalf("list_capabilities should be part of the immediate capability kernel: %+v", got)
	}
	if got := byName["list_workers"]; got.LoadMode != string(ai.ToolLoadImmediate) {
		t.Fatalf("list_workers should keep named worker resolution in the immediate capability kernel: %+v", got)
	}
	for _, name := range []string{"search_knowledge", "search_history"} {
		if got := byName[name]; got.LoadMode != string(ai.ToolLoadImmediate) {
			t.Fatalf("%s should keep permission-aware memory retrieval immediately visible: %+v", name, got)
		}
	}
	if got := byName["low_level_db_query"].Domain; got != CapabilityOps {
		t.Fatalf("low_level_db_query domain=%q", got)
	}
	if got := byName["low_level_db_query"].Effect; got != ToolEffectRead {
		t.Fatalf("low_level_db_query effect=%q", got)
	}
	if got := byName["low_level_db_exec"].Effect; got != ToolEffectWrite {
		t.Fatalf("low_level_db_exec effect=%q", got)
	}
	if !byName["low_level_db_exec"].ApprovalRequired || byName["low_level_db_exec"].GroupAllowed {
		t.Fatalf("low_level_db_exec 应需要确认且禁止群聊: %+v", byName["low_level_db_exec"])
	}
	if got := byName["send_message"].Effect; got != ToolEffectWrite {
		t.Fatalf("send_message effect=%q", got)
	}
	if got := byName["create_data_collection_campaign"].Effect; got != ToolEffectWrite {
		t.Fatalf("create_data_collection_campaign effect=%q", got)
	}
	if got := byName["run_worker_command"].Effect; got != ToolEffectExecute {
		t.Fatalf("run_worker_command effect=%q", got)
	}
	if got := byName["run_worker_command"].Description; !strings.Contains(got, "不会启动 Codex/Claude") || !strings.Contains(got, "delegate_worker_agent") {
		t.Fatalf("run_worker_command 没有明确原子命令边界: %q", got)
	}
	if got := byName["delegate_worker_agent"]; got.Effect != ToolEffectExecute || got.RequiredAction != perm.ActManageWorker || got.GroupAllowed {
		t.Fatalf("delegate_worker_agent 元数据错误: %+v", got)
	}
	if got := byName["delegate_worker_agent"]; got.LoadMode != string(ai.ToolLoadImmediate) {
		t.Fatalf("delegate_worker_agent should keep adaptive delegation immediately visible: %+v", got)
	}
	if got := byName["run_worker_command"]; got.LoadMode == string(ai.ToolLoadImmediate) {
		t.Fatalf("low-level command execution should remain deferred: %+v", got)
	}
	for _, name := range []string{"run_worker_command", "delegate_worker_agent", "analyze_company_materials", "start_worker_skill", "start_workflow"} {
		if got := byName[name].Completion; got != string(ai.ToolCompletionAsynchronous) {
			t.Fatalf("%s completion=%q", name, got)
		}
	}
	if !byName["delete_project"].ApprovalRequired {
		t.Fatalf("delete_project 应标记为审批工具")
	}
	if byName["start_workflow"].GroupAllowed {
		t.Fatalf("start_workflow 不应在群共享会话可用")
	}
}

func TestFixedToolInputsExposeJSONSchemaEnums(t *testing.T) {
	super := &store.User{ID: 1, Name: "boss", Status: store.UserActive, IsSuperadmin: true}
	byName := map[string]ai.Tool{}
	for _, item := range baseStaticTools(Deps{}, super) {
		byName[item.Name] = item
	}
	status := schemaProperties(byName["update_my_task_status"].InputSchema)["status"].(map[string]any)
	if got := fmt.Sprint(status["enum"]); got != "[pending in_progress done]" {
		t.Fatalf("task status enum = %s", got)
	}
	workflow := schemaProperties(byName["start_workflow"].InputSchema)["name"].(map[string]any)
	if len(workflow["enum"].([]string)) != 3 {
		t.Fatalf("workflow enum = %#v", workflow["enum"])
	}
	if _, exists := byName["schedule_push"]; exists {
		t.Fatal("ambiguous schedule_push must not remain exposed")
	}
	if byName["schedule_once_push"].Name == "" || byName["schedule_recurring_push"].Name == "" {
		t.Fatal("split schedule tools are missing")
	}
}

func TestNormalizeAgentScope(t *testing.T) {
	typ, key, message := normalizeAgentScope(" Repo : nbco ")
	if message != "" || typ != "repo" || key != "repo:nbco" {
		t.Fatalf("scope = (%q, %q, %q)", typ, key, message)
	}
	for _, invalid := range []string{"", "nbco", ":nbco", "repo:", "bad type:value", "repo:\nvalue"} {
		if _, _, message := normalizeAgentScope(invalid); message == "" {
			t.Fatalf("invalid scope %q was accepted", invalid)
		}
	}
}

func TestBuiltinToolRegistryIsCompleteAndUnique(t *testing.T) {
	super := &store.User{ID: 1, Name: "boss", Status: store.UserActive, IsSuperadmin: true}
	seen := map[string]bool{}
	for _, tl := range baseStaticTools(Deps{}, super) {
		if seen[tl.Name] {
			t.Errorf("内建工具重名: %s", tl.Name)
		}
		seen[tl.Name] = true
		if _, ok := toolEffect[tl.Name]; !ok {
			t.Errorf("内建工具未声明 effect: %s", tl.Name)
		}
	}
}

func TestDedupeToolsKeepsFirstDefinition(t *testing.T) {
	got := dedupeTools([]ai.Tool{
		{Name: "same", Description: "trusted"},
		{Name: "same", Description: "external"},
		{Name: "other"},
	})
	if len(got) != 2 || got[0].Description != "trusted" || got[1].Name != "other" {
		t.Fatalf("工具去重结果错误: %+v", got)
	}
}

func TestRenderActionTurnsIncludesToolEvidence(t *testing.T) {
	got := renderActionTurns(context.Background(), nil, time.UTC, []*store.ActionTurn{{
		ID:               9,
		UserID:           1,
		Channel:          "telegram",
		UserTextExcerpt:  "给大家发通知",
		ReplyExcerpt:     "已发送。",
		RequiresAction:   true,
		Intent:           "发送通知",
		ExpectedTools:    []string{"send_message"},
		Evidence:         json.RawMessage(`{"tool_evidence":[{"tool":"send_message","ok":true,"summary":"已发送给 3 人。"}],"turn_context":{"route":"people,action","tool_count":22,"full_tool_count":152,"tool_schema_chars":12345,"system_chars":6789,"history_chars":321,"agent_iterations":3,"model_calls":4},"finish_reason":"stop"}`),
		Outcome:          "evidence_ok",
		ToolCount:        1,
		SuccessToolCount: 1,
		CreatedAt:        time.Date(2026, 7, 9, 20, 30, 0, 0, time.UTC),
	}})
	for _, want := range []string{"历史记录：曾判定已执行", "handler 返回 1/1", "发送通知", "实际动作工具", "send_message:returned", "已发送给 3 人", "route=people,action", "catalog=22/152", "model_calls=4", "finish_reason"} {
		if !strings.Contains(got, want) {
			t.Fatalf("动作账本渲染缺 %q:\n%s", want, got)
		}
	}
}

func TestRenderSystemActivityPreservesBusinessResultSemantics(t *testing.T) {
	empty := renderSystemActivity(time.UTC, nil)
	if !strings.Contains(empty, "当前筛选条件和时间范围") || !strings.Contains(empty, "不能证明") {
		t.Fatalf("空活动结果必须保留证据范围: %s", empty)
	}
	got := renderSystemActivity(time.UTC, []*store.AuditActivity{
		{
			ID: 12, UserID: 7, UserName: "X Fan", Tool: "update_my_profile",
			Args: []byte(`{"fields":{"职位":"开发"}}`), Result: "已更新。", OK: true,
			CreatedAt: time.Date(2026, 7, 10, 7, 43, 6, 0, time.UTC),
		},
		{
			ID: 11, UserID: 6, UserName: "mxb", Tool: "update_my_profile",
			Result: "字段 department 未定义。", OK: true,
			CreatedAt: time.Date(2026, 7, 10, 7, 40, 0, 0, time.UTC),
		},
	})
	for _, want := range []string{"系统工具调用流水", "X Fan", "update_my_profile", "职位", "已更新", "department 未定义", "业务结果以结果正文为准"} {
		if !strings.Contains(got, want) {
			t.Fatalf("系统活动渲染缺 %q:\n%s", want, got)
		}
	}
}

func TestStaticToolDomainsRegistered(t *testing.T) {
	super := &store.User{ID: 1, Name: "boss", Status: store.UserActive, IsSuperadmin: true}
	for _, tl := range baseStaticTools(Deps{}, super) {
		if got := capabilityDomain(tl.Name); got == CapabilityExtension {
			t.Errorf("内建工具 %s 缺少明确业务域", tl.Name)
		}
		if got := ToolEffect(tl.Name); got == ToolEffectUnknown {
			t.Errorf("内建工具 %s 缺少 effect 分类", tl.Name)
		}
	}
	if ToolCanProveAction("list_telegram_groups") {
		t.Fatal("读取类工具不能作为动作完成证据")
	}
	if !ToolCanProveAction("set_telegram_group_monitor") {
		t.Fatal("写入类工具应能作为动作完成证据")
	}
	if !ToolCanProveAction("run_worker_command") {
		t.Fatal("执行类工具应能作为动作完成证据")
	}
}

func TestWorkflowTemplatesAndUpgradeCommand(t *testing.T) {
	if _, err := workflowTemplateByName("material_intake"); err != nil {
		t.Fatal(err)
	}
	if _, err := workflowTemplateByName("nbco_upgrade"); err != nil {
		t.Fatal(err)
	}
	if _, err := workflowTemplateByName("nbco_code_change"); err != nil {
		t.Fatal(err)
	}
	rendered := renderWorkflowTemplates("")
	for _, want := range []string{"material_intake", "nbco_upgrade", "nbco_code_change", "confirm"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("工作流列表缺 %q:\n%s", want, rendered)
		}
	}
	cmd := nbcoUpgradeCommand("/root/src/nbco", "origin/main")
	if !strings.Contains(cmd, "cd '/root/src/nbco'") || !strings.Contains(cmd, "scripts/upgrade-nbco.sh 'origin/main'") {
		t.Fatalf("升级命令不对: %s", cmd)
	}
	defaultCmd := nbcoUpgradeCommand("", "")
	if !strings.Contains(defaultCmd, `NBCO_REPO_DIR`) || !strings.Contains(defaultCmd, "origin/main") {
		t.Fatalf("默认升级命令应走环境变量兜底: %s", defaultCmd)
	}
	templates := ListWorkflowTemplates()
	templates[0].Args["file_ids"] = "mutated"
	fresh := ListWorkflowTemplates()
	if fresh[0].Args["file_ids"] == "mutated" {
		t.Fatal("ListWorkflowTemplates must deep-copy Args")
	}
	codePrompt := nbcoCodeChangePrompt("增加天气预报功能", "", "", "", true, true)
	for _, want := range []string{"增加天气预报功能", "agent session scope", "提交", "readyz", "不要泄露"} {
		if !strings.Contains(codePrompt, want) {
			t.Fatalf("nbco code change prompt 缺 %q:\n%s", want, codePrompt)
		}
	}
	prompt := workerSkillTaskPrompt(&store.Knowledge{Title: "通用流程", Content: "执行方法：按步骤做", Tags: []string{"scope:global"}}, "完成本次目标")
	for _, want := range []string{"通用流程", "完成本次目标", "不要泄露密钥"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("worker skill prompt 缺 %q:\n%s", want, prompt)
		}
	}
}

func TestNBCOUpgradeWorkflowRequiresSuperadmin(t *testing.T) {
	user := &store.User{ID: 2, Name: "manager", Status: store.UserActive}
	got, err := StartWorkflow(context.Background(), Deps{}, user, "nbco_upgrade", json.RawMessage(`{"confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "超级管理员") {
		t.Fatalf("nbco_upgrade should reject non-super users before any side effect: %q", got)
	}
	ok, reason, err := CanStartWorkflow(context.Background(), Deps{}, user, "nbco_upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if ok || !strings.Contains(reason, "超级管理员") {
		t.Fatalf("CanStartWorkflow nbco_upgrade = ok=%v reason=%q", ok, reason)
	}
}

func TestNormalizeInfoFieldsAliasesAndNullish(t *testing.T) {
	fields, msg := normalizeInfoFields(map[string]string{
		"外号": "PRO",
		"职位": "岗位：CEO",
		"手机": "null",
	}, []string{"昵称", "职位", "手机"})
	if msg != "" {
		t.Fatalf("normalizeInfoFields msg=%q", msg)
	}
	if fields["昵称"] != "PRO" || fields["职位"] != "CEO" || fields["手机"] != "" {
		t.Fatalf("归一化结果不对: %#v", fields)
	}

	fields, msg = normalizeInfoFields(map[string]string{"昵称": "外号：null"}, []string{"昵称"})
	if msg != "" || fields["昵称"] != "" {
		t.Fatalf("字段前缀里的 null 应清空字段: fields=%#v msg=%q", fields, msg)
	}

	value := "CEO"
	fields, msg = normalizeInfoFieldsPtr(map[string]*string{"职位": &value, "手机": nil}, []string{"职位", "手机"})
	if msg != "" {
		t.Fatalf("normalizeInfoFieldsPtr msg=%q", msg)
	}
	if fields["职位"] != "CEO" || fields["手机"] != "" {
		t.Fatalf("JSON null 应清空字段: %#v", fields)
	}
}

func TestBuildSkillContent(t *testing.T) {
	content, tags, msg := buildSkillContent(skillArgs{
		Title:       "群邀请流程",
		Trigger:     "群里有人要求加入",
		Summary:     "先判断真人员工还是 worker，再走对应邀请路径",
		Procedure:   "1. 查询群成员\n2. 判断身份\n3. 生成一次性邀请",
		Constraints: "不要在群里公开 token",
		Scope:       "telegram",
		Tags:        []string{"邀请", "scope:global"},
	})
	if msg != "" {
		t.Fatalf("buildSkillContent msg=%q", msg)
	}
	for _, want := range []string{"触发条件：群里有人要求加入", "摘要：先判断", "执行方法：", "限制与禁忌："} {
		if !strings.Contains(content, want) {
			t.Fatalf("skill content 缺 %q:\n%s", want, content)
		}
	}
	if strings.Join(tags, ",") != "scope:telegram,邀请" {
		t.Fatalf("tags 应重写 scope 且去重: %#v", tags)
	}
	parts := parseSkillContent(content)
	if parts.Trigger == "" || parts.Summary == "" || !strings.Contains(parts.Procedure, "查询群成员") || !strings.Contains(parts.Constraints, "token") {
		t.Fatalf("parseSkillContent 不完整: %#v", parts)
	}
}

func TestRenderLearningCandidatesIncludesEvidence(t *testing.T) {
	got := renderLearningCandidates([]*store.LearningCandidate{{
		ID:         7,
		Kind:       store.LearningKindRule,
		Status:     store.LearningStatusPending,
		Scope:      "telegram",
		Title:      "Token 不外发",
		Content:    "不要把 worker token 发给用户。",
		Tags:       []string{"scope:telegram"},
		SourceType: "memory_miner",
		SourceRef:  "session:1/message:2",
		Confidence: 0.62,
		Evidence:   json.RawMessage(`{"user_text":"以后不要把 worker token 发出来"}`),
	}})
	for _, want := range []string{"候选内部编号 7", "source: memory_miner session:1/message:2", "confidence: 0.62", "以后不要把 worker token 发出来"} {
		if !strings.Contains(got, want) {
			t.Fatalf("候选渲染缺 %q:\n%s", want, got)
		}
	}
}

func TestFmtTime(t *testing.T) {
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	utc := time.Date(2026, 7, 3, 4, 5, 0, 0, time.UTC)
	if got := fmtTime(utc, tz); got != "2026-07-03 12:05" {
		t.Errorf("fmtTime = %q", got)
	}
}

func TestFormatBytesHuge(t *testing.T) {
	if got := formatBytes(1 << 62); got == "" {
		t.Fatal("formatBytes should not return empty string")
	}
}

func TestObjectRenderersUseInternalRefsInsteadOfHashIDs(t *testing.T) {
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	got := strings.Join([]string{
		renderProjects([]store.Project{{ID: 1, Name: "视频项目", Status: store.ProjectActive}}),
		renderTasks([]*store.Task{{ID: 2, Status: store.TaskPending, Title: "整理资料"}}, time.UTC),
		renderKnowledgeList([]*store.Knowledge{{ID: 3, Title: "部署流程"}}),
		renderFileList([]store.File{{ID: 4, OriginalName: "roster.xlsx", MIMEType: "application/vnd.ms-excel", SizeBytes: 128, CreatedAt: now}}, time.UTC),
		renderSkillList([]*store.Knowledge{{ID: 5, Title: "升级 SOP", Content: "摘要：稳定升级"}}),
	}, "\n")
	for _, bad := range []string{"#1", "#2", "#3", "#4", "#5", "用户1", "用户2", "ID 1", "ID: 1"} {
		if strings.Contains(got, bad) {
			t.Fatalf("渲染不应使用裸内部编号 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"项目内部编号 1", "任务内部编号 2", "知识内部编号 3", "文件内部编号 4", "skill内部编号 5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("渲染缺 %q:\n%s", want, got)
		}
	}
}

func TestRenderFileQueueDistinguishesSavedAndFailed(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := renderFileQueue(
		[]store.File{{ID: 4, OriginalName: "saved.pdf", SizeBytes: 10, CreatedAt: now}},
		[]store.FileIntake{
			{ID: 1, OriginalName: "saved.pdf", Status: store.FileIntakeSaved, CreatedAt: now},
			{ID: 2, OriginalName: "large.zip", SizeBytes: 25 << 20, Status: store.FileIntakeFailed, ErrorMessage: "Telegram 下载受限", CreatedAt: now},
			{ID: 3, OriginalName: "pending.xlsx", Status: store.FileIntakePending, CreatedAt: now},
		}, time.UTC,
	)
	for _, want := range []string{"文件内部编号 4", "large.zip", "Telegram 下载受限", "pending.xlsx", "仍在接收中", "没有系统 file_id"} {
		if !strings.Contains(got, want) {
			t.Fatalf("queue missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "saved.pdf") != 1 {
		t.Fatalf("saved intake should not duplicate the file row:\n%s", got)
	}
}

func TestRenderWorkspaceResources(t *testing.T) {
	now := time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC)
	got := renderWorkspaceResources([]store.WorkspaceResource{
		{Kind: "file", ID: 4, Name: "申请表.pdf", State: "saved", CreatedAt: now},
		{Kind: "task", ID: 6, Name: "申请表分析", State: "done", CreatedAt: now},
	}, time.UTC, semanticSearchPlan{Terms: []string{"申请表"}}, false)
	for _, want := range []string{"resource_ref=file:4", "类型=file", "申请表.pdf", "resource_ref=task:6", "不要把内部引用主动展示"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace rendering missing %q:\n%s", want, got)
		}
	}
}

func TestPlanSemanticSearchUsesAIInsteadOfCodeFuzzyMatching(t *testing.T) {
	called := false
	d := Deps{SubcallAI: func(_ context.Context, _ *store.User, req SubcallRequest) (string, error) {
		called = true
		if req.Purpose != "search_planner" {
			t.Fatalf("purpose = %q", req.Purpose)
		}
		if !strings.Contains(req.Prompt, "无成分陪伴") {
			t.Fatalf("planner did not receive intent: %s", req.Prompt)
		}
		return `{"terms":["无成人陪伴","乘机申请表"],"kinds":["file"],"recent":false}`, nil
	}}
	plan := planSemanticSearch(context.Background(), d, &store.User{ID: 1}, "无成分陪伴的那个文件", []string{"task", "file", "project"})
	if !called {
		t.Fatal("semantic planner was not called")
	}
	if got := strings.Join(plan.Terms, ","); got != "无成人陪伴,乘机申请表" {
		t.Fatalf("terms = %q", got)
	}
	if len(plan.Kinds) != 1 || plan.Kinds[0] != "file" || plan.Recent {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanSemanticSearchKeepsObjectTermsForRecentIntent(t *testing.T) {
	d := Deps{SubcallAI: func(context.Context, *store.User, SubcallRequest) (string, error) {
		return `{"terms":["视频项目"],"kinds":["files"],"recent":true}`, nil
	}}
	plan := planSemanticSearch(context.Background(), d, &store.User{ID: 1}, "视频项目最新文件", []string{"files", "tasks"})
	if !plan.Recent || len(plan.Terms) != 1 || plan.Terms[0] != "视频项目" {
		t.Fatalf("recent object plan = %+v", plan)
	}
}

func TestParseSemanticSearchPlanRejectsInventedKindsWithoutTreatingTermsAsSQL(t *testing.T) {
	plan, ok := parseSemanticSearchPlan(`{"terms":["申请表","%全部%","申请表"],"kinds":["file","secret"],"recent":false}`, []string{"task", "file"})
	if !ok {
		t.Fatal("valid JSON plan was rejected")
	}
	if len(plan.Terms) != 2 || plan.Terms[0] != "申请表" || plan.Terms[1] != "%全部%" {
		t.Fatalf("terms = %#v", plan.Terms)
	}
	if len(plan.Kinds) != 1 || plan.Kinds[0] != "file" {
		t.Fatalf("kinds = %#v", plan.Kinds)
	}
}

func TestParseSemanticSearchPlanBoundsBroadSourceSelection(t *testing.T) {
	allowed := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	plan, ok := parseSemanticSearchPlan(`{"kinds":["a","b","c","d","e","f","g","h","i","j"]}`, allowed)
	if !ok || len(plan.Kinds) != 8 {
		t.Fatalf("plan = %+v, ok=%t", plan, ok)
	}
}

func TestRenderDataSourcesAndRows(t *testing.T) {
	catalog := renderDataSources([]store.DataSource{{Name: "tasks", Description: "任务", Fields: []string{"task_id", "status"}}})
	for _, want := range []string{"tasks", "task_id,status", "当前身份"} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("catalog missing %q: %s", want, catalog)
		}
	}
	rows := renderDataRows("tasks", semanticSearchPlan{Terms: []string{"申请表"}}, map[string]string{"status": "done"}, 0,
		[]json.RawMessage{json.RawMessage(`{"task_id":6,"status":"done"}`)})
	for _, want := range []string{"数据源=tasks", `"task_id":6`, "权限裁剪"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("rows missing %q: %s", want, rows)
		}
	}
}

func TestCanonicalArgsHash(t *testing.T) {
	a := canonicalArgsHash([]byte(`{"user_id": 5, "reason": "违规"}`))
	b := canonicalArgsHash([]byte(`{ "reason":"违规","user_id":5 }`))
	if a != b {
		t.Error("键序/空白不同但语义相同的参数应哈希一致")
	}
	c := canonicalArgsHash([]byte(`{"user_id": 6, "reason": "违规"}`))
	if a == c {
		t.Error("参数不同应哈希不同")
	}
	if canonicalArgsHash(nil) == "" {
		t.Error("空参数也应有稳定哈希")
	}
}

func TestAsynchronousToolCoalescesIdenticalCallsPerToolset(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	task := asynchronousTool(tool("async_test", "test", obj(map[string]any{
		"worker_id": p("integer", "worker"),
		"title":     p("string", "title"),
	}, "worker_id", "title"), func(context.Context, json.RawMessage) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return `{"status":"accepted"}`, nil
	}))

	type result struct {
		text string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		text, err := task.Handler(context.Background(), json.RawMessage(`{"worker_id":2,"title":"same"}`))
		results <- result{text: text, err: err}
	}()
	<-started
	go func() {
		text, err := task.Handler(context.Background(), json.RawMessage(`{"title":"same", "worker_id":2}`))
		results <- result{text: text, err: err}
	}()
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || got.text != `{"status":"accepted"}` {
			t.Fatalf("coalesced result = %q, %v", got.text, got.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("identical concurrent calls executed %d times", calls.Load())
	}
	if _, err := task.Handler(context.Background(), json.RawMessage(`{"worker_id":2,"title":"same"}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("identical sequential call executed again: %d", calls.Load())
	}
	if _, err := task.Handler(context.Background(), json.RawMessage(`{"worker_id":2,"title":"different"}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("different arguments should execute independently: %d", calls.Load())
	}
}

func TestTelegramUserSelectorParsing(t *testing.T) {
	for _, in := range []string{"tg:6103874246", "tgid:6103874246", "tg_id:6103874246", "telegram:6103874246", "telegram_id:6103874246"} {
		got, ok := parseTelegramUserSelector(in)
		if !ok || got != "6103874246" {
			t.Fatalf("parseTelegramUserSelector(%q) = %q,%v", in, got, ok)
		}
	}
	if _, ok := parseTelegramUserSelector("6103874246"); ok {
		t.Fatal("裸数字不是显式 Telegram selector，应由 resolveUserArg 先按内部ID再 fallback")
	}
	if got := telegramUserSelector(" 6103874246 "); got != "tg:6103874246" {
		t.Fatalf("telegramUserSelector trim = %q", got)
	}
}

func TestApprovalRequiredToolsExist(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	names := map[string]bool{}
	for _, tl := range ForUser(Deps{}, su, nil) {
		names[tl.Name] = true
	}
	for n := range approvalRequired {
		if !names[n] {
			t.Errorf("approvalRequired 登记了不存在的工具 %s", n)
		}
	}
}

func TestReadOnlyToolsExcludesWritesExecutorsAndUnknownExtensions(t *testing.T) {
	in := []ai.Tool{
		{Name: "get_my_tasks", Effect: ai.ToolEffectRead},
		{Name: "send_message", Effect: ai.ToolEffectWrite},
		{Name: "score_learning_candidates"},
		{Name: "custom_executor", Effect: ai.ToolEffectExecute},
		{Name: "unknown_extension"},
	}
	got := ReadOnlyTools(in)
	if len(got) != 1 || got[0].Name != "get_my_tasks" {
		t.Fatalf("read-only tools = %v", namesOf(got))
	}
}
