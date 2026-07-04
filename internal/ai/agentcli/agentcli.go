// Package agentcli 是驱动 agent CLI（claude、codex，未来任意同类）的通用引擎模板。
//
// 模式：每轮 exec 一次 CLI 子进程（headless + JSONL 输出 + 会话 resume），
// 工具通过本进程的 HTTP MCP 端点回连暴露 —— 每轮生成一次性 token 注册
// 用户裁剪后的工具集，token 嵌在回连 URL 路径里（对 header 支持参差的
// CLI 一视同仁），轮次结束即注销，工具调用天然带用户域隔离。
//
// 引擎与 CLI 的差异全部收敛在 Driver 接口：怎么拼命令行、怎么解析输出。
// 会话由 CLI 自管（resume），EngineSession 即 CLI 侧会话标识；
// 消息与工具轨迹仍由调用方落库审计。
package agentcli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/mcpbridge"
)

// Plan 一轮子进程调用的完整方案，由 Driver 构造。
type Plan struct {
	Bin  string
	Args []string
	Env  []string // 追加到继承环境之上
	Dir  string   // 工作目录（须已存在）
	// Consume 消费 stdout 的每一行（已裁剪空白，非空）。
	Consume func(line []byte)
	// Finish 进程退出后取结果。waitErr 是 cmd.Wait 的错误，stderr 是错误输出。
	Finish func(waitErr error, stderr string) (*ai.TurnResult, error)
}

// Driver 一种 agent CLI 的接线方式。
type Driver interface {
	// Name 引擎标识（写进配置与会话记录）。
	Name() string
	// Plan 构造一轮调用。mcpURL 是本轮回连端点（含一次性 token）。
	// 用户输入统一经 stdin 传入，Driver 的命令行需以 stdin 作为提示词来源。
	Plan(req *ai.TurnRequest, mcpURL string) (*Plan, error)
}

// Engine 通用 CLI 引擎，实现 ai.Engine。
type Engine struct {
	driver   Driver
	registry *Registry
	// callbackBase 回连端点前缀（不含 token），如 http://127.0.0.1:8900/mcp/cli
	callbackBase string
}

// NewEngine 组装引擎。
func NewEngine(driver Driver, registry *Registry, callbackBase string) *Engine {
	return &Engine{driver: driver, registry: registry, callbackBase: strings.TrimRight(callbackBase, "/")}
}

// Name 实现 ai.Engine。
func (e *Engine) Name() string { return e.driver.Name() }

// RunTurn 实现 ai.Engine。
func (e *Engine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	rec := &recorder{onEvent: req.OnEvent}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	e.registry.register(token, wrapTools(req.Tools, rec))
	defer e.registry.unregister(token)

	plan, err := e.driver.Plan(req, e.callbackBase+"/"+token)
	if err != nil {
		return nil, err
	}

	slog.Debug("CLI 引擎启动", "bin", plan.Bin, "session", req.SessionID,
		"resume", req.EngineSession != "", "tools", len(req.Tools))
	started := time.Now()

	cmd := exec.CommandContext(ctx, plan.Bin, plan.Args...)
	cmd.Dir = plan.Dir
	if len(plan.Env) > 0 {
		cmd.Env = append(os.Environ(), plan.Env...)
	}
	cmd.Stdin = strings.NewReader(req.UserText)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s: %w", plan.Bin, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 {
			plan.Consume(line)
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	slog.Debug("CLI 引擎退出", "session", req.SessionID, "dur", time.Since(started).Round(time.Millisecond),
		"wait_err", waitErr != nil, "stderr", truncate(stderr.String(), 300))
	if scanErr != nil {
		return nil, fmt.Errorf("读取 CLI 输出: %w", scanErr)
	}
	res, err := plan.Finish(waitErr, truncate(stderr.String(), 2000))
	if err != nil {
		return nil, err
	}
	res.Steps = rec.snapshot()
	return res, nil
}

// --- 回连注册表 ---

// Registry 一次性 token → 工具集。所有 CLI 驱动共用同一个回连端点。
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

// Handler 回连 MCP 端点。token 取 URL 最后一段（兼容取 Bearer header），
// 无效一律拒绝。挂载到以 /mcp/cli/ 为前缀的子树。
func (r *Registry) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		token := path.Base(req.URL.Path)
		if _, ok := r.lookup(token); !ok {
			token = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		}
		tools, ok := r.lookup(token)
		if !ok {
			return nil
		}
		return mcpbridge.NewServer("nbco", "1", tools)
	}, nil)
}

// --- 工具轨迹 ---

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

// --- 小工具 ---

// workDir 引擎的稳定工作目录（CLI 会话状态依赖目录稳定）。
func workDir(sub ...string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("取用户缓存目录: %w", err)
	}
	dir := filepath.Join(append([]string{cache, "nbco"}, sub...)...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建工作目录 %s: %w", dir, err)
	}
	return dir, nil
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
