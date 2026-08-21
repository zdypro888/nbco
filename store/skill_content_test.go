package store

import (
	"errors"
	"strings"
	"testing"
)

func TestSkillContentRoundTrip(t *testing.T) {
	want := NewSkillContent("群里有人申请加入", "核实身份后走对应邀请流程", "1. 查询身份\n2. 创建一次性邀请", "不得公开凭据")
	raw, err := EncodeSkillContent(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "触发条件：") {
		t.Fatalf("durable content must not depend on presentation labels: %s", raw)
	}
	got, err := DecodeSkillContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	rendered := RenderSkillContent(got)
	for _, wantText := range []string{"触发条件：群里有人申请加入", "摘要：核实身份", "执行方法：", "限制与禁忌："} {
		if !strings.Contains(rendered, wantText) {
			t.Fatalf("rendered skill missing %q: %s", wantText, rendered)
		}
	}
}

func TestDecodeSkillContentIsStrict(t *testing.T) {
	tests := []string{
		`{"version":1,"trigger":"t","summary":"s","procedure":"p","constraints":"","extra":true}`,
		`{"version":1,"trigger":"t","summary":"s","procedure":"p","constraints":""} {}`,
		`{"version":2,"trigger":"t","summary":"s","procedure":"p","constraints":""}`,
		`{"version":1,"trigger":"","summary":"s","procedure":"p","constraints":""}`,
		"触发条件：t\n摘要：s\n执行方法：p",
	}
	for _, raw := range tests {
		if _, err := DecodeSkillContent(raw); !errors.Is(err, ErrInvalidSkillContent) {
			t.Fatalf("DecodeSkillContent(%q) err=%v", raw, err)
		}
	}
}
