// Package claudecli 用 claude CLI（headless）实现 ai.Engine。
//
// 模式（参考 aibridge 的 MCP 回连）：每轮 exec 一次
//
//	claude -p --output-format stream-json --resume <会话>
//
// 工具通过本进程的 HTTP MCP 端点回连暴露：每轮生成一次性 token 注册用户裁剪后的
// 工具集，CLI 带该 token 连回来，轮次结束即注销 —— 工具调用天然带用户域隔离。
// 内置工具全部禁用（--tools ""），CLI 只当带 tool 循环的对话模型用。
//
// 会话由 CLI 自管（--resume），EngineSession 即 CLI session id；
// 消息与工具轨迹仍由调用方落库审计。
package claudecli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/config"
	"github.com/zdypro888/nbco/internal/mcpbridge"
)

// Registry 是回连端点的一次性 token → 工具集注册表。
type Registry struct {
	mu    sync.Mutex
	turns map[string][]ai.Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{turns: map[string][]ai.Tool{}}
}

func (r *Registry) register(token string, tools []ai.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns[token] = tools
}

func (r *Registry) unregister(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.turns, token)
}

func (r *Registry) lookup(token string) ([]ai.Tool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tools, ok := r.turns[token]
	return tools, ok
}

// Handler 返回回连 MCP 端点的 http.Handler；无效 token 一律拒绝。
func (r *Registry) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			return nil
		}
		tools, ok := r.lookup(token)
		if !ok {
			return nil
		}
		return mcpbridge.NewServer("nbco", "1", tools)
	}, nil)
}

// Engine 实现 ai.Engine。
type Engine struct {
	registry    *Registry
	cmd         string
	model       string
	callbackURL string // 回连 MCP 端点完整 URL
	workDir     string // CLI 会话状态目录（--resume 依赖工作目录稳定）
}

// New 创建引擎。callbackURL 形如 http://127.0.0.1:8900/mcp/cli。
func New(cfg config.AIConfig, registry *Registry, callbackURL string) (*Engine, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("取用户缓存目录: %w", err)
	}
	workDir := filepath.Join(cache, "nbco-claude")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 CLI 工作目录: %w", err)
	}
	return &Engine{
		registry:    registry,
		cmd:         cfg.ClaudeCmd,
		model:       cfg.Model,
		callbackURL: callbackURL,
		workDir:     workDir,
	}, nil
}

// Name 实现 ai.Engine。
func (e *Engine) Name() string { return config.EngineClaudeCLI }

// RunTurn 实现 ai.Engine。
func (e *Engine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	rec := &recorder{onEvent: req.OnEvent}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	e.registry.register(token, wrapTools(req.Tools, rec))
	defer e.registry.unregister(token)

	mcpCfg, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"nbco": map[string]any{
				"type":    "http",
				"url":     e.callbackURL,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--system-prompt", req.System,
		"--tools", "",
		"--allowedTools", "mcp__nbco",
		"--mcp-config", string(mcpCfg),
		"--strict-mcp-config",
		"--setting-sources", "",
	}
	if e.model != "" {
		args = append(args, "--model", e.model)
	}
	if req.EngineSession != "" {
		args = append(args, "--resume", req.EngineSession)
	}

	cmd := exec.CommandContext(ctx, e.cmd, args...)
	cmd.Dir = e.workDir
	cmd.Stdin = strings.NewReader(req.UserText)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s: %w", e.cmd, err)
	}

	res, parseErr := e.consume(stdout)
	waitErr := cmd.Wait()
	if parseErr != nil {
		return nil, parseErr
	}
	if res == nil {
		if waitErr != nil {
			return nil, fmt.Errorf("claude CLI 失败: %w; stderr: %s", waitErr, truncate(stderr.String(), 2000))
		}
		return nil, fmt.Errorf("claude CLI 未输出 result 事件; stderr: %s", truncate(stderr.String(), 2000))
	}
	res.Steps = rec.snapshot()
	return res, nil
}

// cliEvent 是 stream-json 行的最小投影。
type cliEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	Usage     *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (e *Engine) consume(r interface{ Read([]byte) (int, error) }) (*ai.TurnResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var res *ai.TurnResult
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev cliEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // 容忍非 JSON 行（CLI 偶发告警输出）
		}
		if ev.Type != "result" {
			continue
		}
		if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
			return nil, fmt.Errorf("claude CLI 轮次失败(%s): %s", ev.Subtype, truncate(ev.Result, 2000))
		}
		res = &ai.TurnResult{
			Text:          strings.TrimSpace(ev.Result),
			EngineSession: ev.SessionID,
		}
		if ev.Usage != nil {
			res.Usage = ai.Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 CLI 输出: %w", err)
	}
	return res, nil
}

// recorder 收集本轮工具轨迹（MCP 回连的调用在任意 goroutine 到达）。
type recorder struct {
	mu      sync.Mutex
	steps   []ai.Step
	onEvent func(ai.Step)
}

func (r *recorder) add(s ai.Step) {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	cb := r.onEvent
	r.mu.Unlock()
	if cb != nil {
		cb(s)
	}
}

func (r *recorder) snapshot() []ai.Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ai.Step(nil), r.steps...)
}

// wrapTools 在 handler 外包一层轨迹记录。
func wrapTools(tools []ai.Tool, rec *recorder) []ai.Tool {
	wrapped := make([]ai.Tool, len(tools))
	for i, t := range tools {
		inner := t.Handler
		name := t.Name
		t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
			out, err := inner(ctx, args)
			step := ai.Step{Kind: ai.StepToolCall, ToolName: name, Args: args, Result: out}
			if err != nil {
				step.Err = err.Error()
			}
			rec.add(step)
			return out, err
		}
		wrapped[i] = t
	}
	return wrapped
}

func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
