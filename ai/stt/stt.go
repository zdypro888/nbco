// Package stt 语音转写（OpenAI 兼容 /audio/transcriptions，如本地 whisper 服务）。
// 可选组件：未配置 stt_model 时为 nil，语音消息提示用户改用文字。
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/zdypro888/nbco/config"
)

// transcribeTimeout 单次转写墙钟上限（语音条通常几十秒，本地 whisper 很快）。
const transcribeTimeout = 2 * time.Minute

// Client 语音转写客户端。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New 按配置构建；未配置 stt_model 返回 nil（调用方按未启用处理）。
// 主引擎为 OpenAI 兼容时，base_url/api_key 空可回退主引擎配置；主引擎为
// Claude/Anthropic 兼容时必须显式配置 stt_base_url。
func New(cfg config.AIConfig) *Client {
	model := strings.TrimSpace(cfg.STTModel)
	if model == "" {
		return nil
	}
	base := strings.TrimSpace(cfg.STTBaseURL)
	if base == "" && cfg.Provider == config.ProviderOpenAI {
		base = strings.TrimSpace(cfg.BaseURL)
	}
	key := strings.TrimSpace(cfg.STTAPIKey)
	if key == "" && cfg.Provider == config.ProviderOpenAI {
		key = strings.TrimSpace(cfg.APIKey)
	}
	if base == "" {
		return nil
	}
	return &Client{baseURL: strings.TrimRight(base, "/"), apiKey: key, model: model,
		http: &http.Client{Timeout: transcribeTimeout}}
}

// Transcribe 把音频转成文字。
func (c *Client) Transcribe(ctx context.Context, filename string, audio io.Reader) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", err
	}
	if err := mw.WriteField("model", c.model); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("转写服务 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析转写响应失败: %w", err)
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", fmt.Errorf("转写结果为空")
	}
	return text, nil
}
