package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zdypro888/nbco/store"
)

func (s *Server) handleAdminAISettings(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.aiSettingsPayload(r.Context()))
}

func (s *Server) handleAdminSetAISettings(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	var req struct {
		Model           *string `json:"model"`
		StreamReasoning *bool   `json:"stream_reasoning"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 无效"})
		return
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		if model != "" {
			if !validRuntimeModelName(model) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "模型名不合法"})
				return
			}
			loaded, err := s.loadedRuntimeModels(r.Context())
			if err != nil {
				slog.Warn("HTTP 查询已加载模型失败，拒绝切换", "user", u.ID, "err", err)
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "暂时无法读取已加载模型列表，未切换"})
				return
			}
			if !runtimeModelInList(model, loaded) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "这个模型当前没有加载，未切换"})
				return
			}
		}
		if err := s.store.SetKV(r.Context(), store.KVAIModel, model); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存模型设置失败"})
			return
		}
		slog.Info("HTTP 超管切换运行时模型", "user", u.ID, "model", model)
	}
	if req.StreamReasoning != nil {
		value := "0"
		if *req.StreamReasoning {
			value = "1"
		}
		if err := s.store.SetKV(r.Context(), store.KVAIStreamReasoning, value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存推理展示设置失败"})
			return
		}
	}
	writeJSON(w, http.StatusOK, s.aiSettingsPayload(r.Context()))
}

func (s *Server) aiSettingsPayload(ctx context.Context) map[string]any {
	runtimeModel, err := s.store.GetKV(ctx, store.KVAIModel)
	if err != nil {
		slog.Warn("读取运行时模型失败", "err", err)
		runtimeModel = ""
	}
	streamRaw, err := s.store.GetKV(ctx, store.KVAIStreamReasoning)
	if err != nil {
		slog.Warn("读取推理展示设置失败", "err", err)
		streamRaw = ""
	}
	loaded, loadedErr := s.loadedRuntimeModels(ctx)
	budget, budgetField := s.llmOutputBudget()
	out := map[string]any{
		"default_model":           strings.TrimSpace(s.llm.Model),
		"runtime_model":           strings.TrimSpace(runtimeModel),
		"current_model":           s.runtimeLLMModel(ctx),
		"output_budget_tokens":    budget,
		"output_budget_api_field": budgetField,
		"max_tokens":              s.llm.MaxTokens,
		"max_completion_tokens":   s.llm.MaxCompletionTokens,
		"reasoning_effort":        strings.TrimSpace(s.llm.ReasoningEffort),
		"stream_reasoning":        store.BoolSetting(streamRaw, s.deps.AIStreamReasoningDefault),
		"stream_reasoning_source": "default",
		"loaded_models":           loaded,
		"model_base_configured":   strings.TrimSpace(s.llm.BaseURL) != "",
	}
	if strings.TrimSpace(streamRaw) != "" {
		out["stream_reasoning_source"] = "runtime"
	}
	if loadedErr != nil {
		out["loaded_models_error"] = loadedErr.Error()
	}
	return out
}

func (s *Server) loadedRuntimeModels(ctx context.Context) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.llm.BaseURL), "/")
	if base == "" {
		return nil, errors.New("ai base_url 未配置")
	}
	base = strings.TrimSuffix(base, "/v1")
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}
	// /v1/models 是“可用模型目录”，不是当前已 launch/loaded 的模型。
	// ai.im.app（exo）暴露 Ollama-compatible /ollama/api/ps 作为运行态模型列表：
	// 这里使用的是稳定兼容 API 面，不表示中枢依赖 Ollama 实现，也不读取 /state 私有接口。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ollama/api/ps", nil)
	if err != nil {
		return nil, err
	}
	if key := strings.TrimSpace(s.llm.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("loaded models status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		name := strings.TrimSpace(m.Model)
		if name == "" {
			name = strings.TrimSpace(m.Name)
		}
		if name == "" || seen[name] || !validRuntimeModelName(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func runtimeModelInList(name string, models []string) bool {
	for _, m := range models {
		if name == m {
			return true
		}
	}
	return false
}

func validRuntimeModelName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 160 || len(strings.Fields(s)) != 1 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == '<' || r == '>' || r == '&' {
			return false
		}
	}
	return true
}
