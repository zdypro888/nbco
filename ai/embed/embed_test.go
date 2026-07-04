package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zdypro888/nbco/config"
)

func TestEmbedParsesAndReorders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// 故意乱序返回，验证按 index 归位。
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float32{0.3, 0.4}},
			{"index": 0, "embedding": []float32{0.1, 0.2}},
		}})
	}))
	defer srv.Close()

	c, err := New(config.AIConfig{EmbedModel: "m", EmbedBaseURL: srv.URL + "/v1", EmbedAPIKey: "k"})
	if err != nil || c == nil {
		t.Fatalf("New = %v %v", c, err)
	}
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][0] != 0.3 {
		t.Fatalf("未按 index 归位: %v", vecs)
	}
}

func TestNewDisabledWhenNoModel(t *testing.T) {
	c, err := New(config.AIConfig{BaseURL: "http://x"})
	if err != nil || c != nil {
		t.Fatalf("无 embed_model 应返回 (nil,nil), got %v %v", c, err)
	}
}
