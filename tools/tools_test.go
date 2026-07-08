package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/nbco/ai"
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

func TestWithTurnBudget(t *testing.T) {
	calls := 0
	ts := WithTurnBudget([]ai.Tool{{
		Name: "lookup",
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "ok", nil
		},
	}}, TurnBudget{MaxCalls: 2, MaxExactRepeat: 1})
	if got, _ := ts[0].Handler(context.Background(), json.RawMessage(`{"a":1}`)); got != "ok" {
		t.Fatalf("首次调用应通过: %q", got)
	}
	if got, _ := ts[0].Handler(context.Background(), json.RawMessage(`{"a":1}`)); !strings.Contains(got, "重复") {
		t.Fatalf("相同参数重复调用应被挡住: %q", got)
	}
	if got, _ := ts[0].Handler(context.Background(), json.RawMessage(`{"a":2}`)); got != "ok" {
		t.Fatalf("不同参数第二次调用应通过: %q", got)
	}
	if got, _ := ts[0].Handler(context.Background(), json.RawMessage(`{"a":3}`)); !strings.Contains(got, "达到上限") {
		t.Fatalf("总预算超限应被挡住: %q", got)
	}
	if calls != 2 {
		t.Fatalf("实际 handler 调用次数 = %d, want 2", calls)
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
