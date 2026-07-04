package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/internal/store"
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
