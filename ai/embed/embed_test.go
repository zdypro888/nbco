package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/zdypro888/nbco/config"
)

// 用 httptest 假一个 OpenAI 兼容 /v1/embeddings，验证经 eino 组件端到端拿到向量。
func TestEmbedThroughEino(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  "m",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}},
				{"object": "embedding", "index": 1, "embedding": []float64{0.4, 0.5, 0.6}},
			},
			"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2},
		})
	}))
	defer srv.Close()

	c, err := New(config.AIConfig{EmbedModel: "m", EmbedBaseURL: srv.URL + "/v1", EmbedAPIKey: "k"})
	if err != nil || c == nil {
		t.Fatalf("New = %v %v", c, err)
	}
	if c.Model() != "m" {
		t.Errorf("Model = %q", c.Model())
	}
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 || vecs[0][0] != 0.1 || vecs[1][2] != 0.6 {
		t.Fatalf("向量不对: %v", vecs)
	}
}

func TestNewDisabledWhenNoModel(t *testing.T) {
	c, err := New(config.AIConfig{BaseURL: "http://x"})
	if err != nil || c != nil {
		t.Fatalf("无 embed_model 应返回 (nil,nil), got %v %v", c, err)
	}
}

// 真 exo 冒烟：设 NBCO_SMOKE_EMBED_BASE / _MODEL / _KEY 时打真实端点，验证
// eino 组件对 base_url(含 /v1) 的拼接与真服务连通。默认跳过。
func TestSmokeRealEmbed(t *testing.T) {
	base := os.Getenv("NBCO_SMOKE_EMBED_BASE")
	model := os.Getenv("NBCO_SMOKE_EMBED_MODEL")
	if base == "" || model == "" {
		t.Skip("设 NBCO_SMOKE_EMBED_BASE / _MODEL(/ _KEY) 跑真端点冒烟")
	}
	c, err := New(config.AIConfig{EmbedModel: model, EmbedBaseURL: base, EmbedAPIKey: os.Getenv("NBCO_SMOKE_EMBED_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := c.Embed(context.Background(), []string{"数据库怎么备份", "前端样式"})
	if err != nil {
		t.Fatalf("真端点 Embed 失败: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) == 0 {
		t.Fatalf("向量异常: %d 条", len(vecs))
	}
	t.Logf("冒烟通过：模型 %s，维度 %d", model, len(vecs[0]))
}
