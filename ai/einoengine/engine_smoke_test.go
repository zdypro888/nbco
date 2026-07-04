package einoengine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

// 真 exo chat 冒烟：设 NBCO_SMOKE_CHAT=1 时用 nbco.json 的 ai 配置跑一轮，验证
// 升级 eino 后中枢引擎（含 tool 循环链路）仍能出结果。默认跳过。
func TestSmokeRealChat(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_CHAT") == "" {
		t.Skip("设 NBCO_SMOKE_CHAT=1 + NBCO_SMOKE_* 跑真端点 chat 冒烟")
	}
	cfg := config.AIConfig{
		Engine:   "eino",
		Provider: os.Getenv("NBCO_SMOKE_PROVIDER"),
		APIKey:   os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL:  os.Getenv("NBCO_SMOKE_BASE"),
		Model:    os.Getenv("NBCO_SMOKE_MODEL"),
	}
	eng, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.RunTurn(context.Background(), &ai.TurnRequest{
		System:   "你是测试助手，简短回答。",
		UserText: "只回复两个字：收到",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if strings.TrimSpace(res.Text) == "" {
		t.Fatal("回复为空")
	}
	t.Logf("chat 冒烟通过：%q", res.Text)
}
