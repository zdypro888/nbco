// Package embed 是 OpenAI 兼容的 embeddings 客户端（走 /v1/embeddings）。
// 指向任何 OpenAI 兼容端点即可：用户的 exo/本地 embedding 服务、云厂商 API。
// nbco 用它把知识、worker 经验向量化做语义检索；未配置时中枢不构建它，检索回退词法。
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zdypro888/nbco/config"
)

// Client 实现 ai.Embedder。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New 按配置建 embedder。embed_base_url / embed_api_key 为空时回退主引擎的
// base_url / api_key（同一个 OpenAI 兼容网关常同时提供 chat 与 embeddings）。
// model 为空返回 (nil, nil) —— 表示未启用语义检索，调用方按 nil 处理。
func New(cfg config.AIConfig) (*Client, error) {
	model := strings.TrimSpace(cfg.EmbedModel)
	if model == "" {
		return nil, nil
	}
	base := strings.TrimSpace(cfg.EmbedBaseURL)
	if base == "" {
		base = cfg.BaseURL
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, fmt.Errorf("embed_model 已设但无 base_url：请配 ai.embed_base_url 或 ai.base_url")
	}
	key := strings.TrimSpace(cfg.EmbedAPIKey)
	if key == "" {
		key = cfg.APIKey
	}
	return &Client{
		baseURL: base,
		apiKey:  key,
		model:   model,
		http:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *Client) Model() string { return c.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed 批量向量化，返回与 texts 一一对应、按 index 归位的向量。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(embedRequest{Model: c.model, Input: texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("embeddings %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings 返回 %d 条，期望 %d", len(er.Data), len(texts))
	}
	// 按 index 归位（有的实现不保证顺序）。
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings 返回非法 index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("embeddings 第 %d 条为空", i)
		}
	}
	return out, nil
}
