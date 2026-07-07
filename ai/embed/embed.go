// Package embed 封装 OpenAI 兼容的 embeddings 客户端，用 eino 的 openai embedding
// 组件实现（与中枢 chat 引擎同一套 eino/acl，风格统一、复用其批处理与错误处理）。
// nbco 用它把知识、worker 经验向量化做语义检索；未配 embed_model 时中枢不构建它，
// 检索回退词法。对外仍是 ai.Embedder（返回 []float32），knowledge/存储层不感知 eino。
package embed

import (
	"context"
	"fmt"
	"strings"
	"time"

	einoembed "github.com/cloudwego/eino-ext/components/embedding/openai"

	"github.com/zdypro888/nbco/config"
)

const embedHTTPTimeout = 60 * time.Second

// Client 实现 ai.Embedder，内部委托 eino 的 openai embedding 组件。
type Client struct {
	model string
	emb   *einoembed.Embedder
}

// New 按配置建 embedder。主引擎为 OpenAI 兼容时，embed_base_url / embed_api_key
// 为空可回退主引擎配置；主引擎为 Claude/Anthropic 兼容时必须显式配置
// embed_base_url，避免把 /embeddings 打到 Anthropic 兼容接口。
// model 为空返回 (nil, nil) —— 表示未启用语义检索，调用方按 nil 处理。
func New(cfg config.AIConfig) (*Client, error) {
	model := strings.TrimSpace(cfg.EmbedModel)
	if model == "" {
		return nil, nil
	}
	base := strings.TrimSpace(cfg.EmbedBaseURL)
	if base == "" && cfg.Provider == config.ProviderOpenAI {
		base = cfg.BaseURL
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, fmt.Errorf("embed_model 已设但无 OpenAI 兼容 base_url：请配 ai.embed_base_url")
	}
	key := strings.TrimSpace(cfg.EmbedAPIKey)
	if key == "" && cfg.Provider == config.ProviderOpenAI {
		key = cfg.APIKey
	}
	emb, err := einoembed.NewEmbedder(context.Background(), &einoembed.EmbeddingConfig{
		APIKey:  key,
		BaseURL: base, // 非 Azure：acl 拼 {base}/embeddings（base 含 /v1 与 chat 一致）
		Model:   model,
		Timeout: embedHTTPTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 embedder: %w", err)
	}
	return &Client{model: model, emb: emb}, nil
}

func (c *Client) Model() string { return c.model }

// Embed 批量向量化，返回与 texts 一一对应的向量（float64→float32，存 real[] 更省）。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	vecs, err := c.emb.EmbedStrings(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embeddings 返回 %d 条，期望 %d", len(vecs), len(texts))
	}
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("embeddings 第 %d 条为空", i)
		}
		f32 := make([]float32, len(v))
		for j, x := range v {
			f32[j] = float32(x)
		}
		out[i] = f32
	}
	return out, nil
}
