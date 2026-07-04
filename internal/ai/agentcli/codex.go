package agentcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zdypro888/nbco/internal/ai"
)

// CodexDriver 驱动 codex CLI（实测 codex-cli 0.142.x）：
//
//	codex exec --json [resume <thread>] -    （提示词经 stdin）
//
// 特性接线：
//   - system prompt：codex 没有对应旗标，写入每个 nbco 会话独占工作目录的
//     AGENTS.md（实测生效），会话隔离天然解决多用户竞争；
//   - MCP：-c mcp_servers.nbco.url=... + features.experimental_use_rmcp_client
//     （参考 aibridge 的兼容做法；token 在 URL 路径里，无需 header）；
//   - --ignore-user-config 屏蔽用户本机配置（认证仍走 CODEX_HOME）；
//   - 最终答复以 --output-last-message 文件为准，JSONL 收集作兜底；
//   - 会话标识取 thread.started 的 thread_id，下一轮 exec resume <id>。
type CodexDriver struct {
	Cmd   string // 可执行文件，默认 codex
	Model string // 可选
}

// Name 实现 Driver。
func (d *CodexDriver) Name() string { return "codexcli" }

// Plan 实现 Driver。
func (d *CodexDriver) Plan(req *ai.TurnRequest, mcpURL string) (*Plan, error) {
	// 每个 nbco 会话一个目录：AGENTS.md 承载 system prompt 且互不串扰。
	dir, err := workDir("codex", "s"+req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(req.System), 0o600); err != nil {
		return nil, fmt.Errorf("写 AGENTS.md: %w", err)
	}
	lastMsg := filepath.Join(dir, "last-message.txt")
	_ = os.Remove(lastMsg) // 残留的上轮输出不可作为本轮兜底

	args := []string{
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"-s", "read-only",
		"-C", dir,
		"-o", lastMsg,
		"-c", "features.experimental_use_rmcp_client=true",
		"-c", fmt.Sprintf("mcp_servers.nbco.url=%q", mcpURL),
		"-c", "approval_policy=never",
	}
	if d.Model != "" {
		args = append(args, "-m", d.Model)
	}
	if req.EngineSession != "" {
		args = append(args, "resume", req.EngineSession)
	}
	args = append(args, "-") // 提示词经 stdin

	var threadID, text, errMsg string
	var usage ai.Usage
	return &Plan{
		Bin:  d.Cmd,
		Args: args,
		Dir:  dir,
		Consume: func(line []byte) {
			var ev struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Message  string `json:"message"`
				Item     *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
				Usage *struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(line, &ev); err != nil {
				return
			}
			switch ev.Type {
			case "thread.started":
				threadID = ev.ThreadID
			case "item.completed":
				if ev.Item != nil && ev.Item.Type == "agent_message" {
					text = ev.Item.Text // 最后一条 agent_message 是最终答复
				}
			case "turn.completed":
				if ev.Usage != nil {
					usage.InputTokens += ev.Usage.InputTokens
					usage.OutputTokens += ev.Usage.OutputTokens
				}
			case "error":
				errMsg = ev.Message
			}
		},
		Finish: func(waitErr error, stderr string) (*ai.TurnResult, error) {
			if waitErr != nil {
				detail := errMsg
				if detail == "" {
					detail = stderr
				}
				return nil, fmt.Errorf("codex CLI 失败: %w; %s", waitErr, truncate(detail, 2000))
			}
			// 最终文本以 -o 文件为准（JSONL 收集兜底）。
			if raw, err := os.ReadFile(lastMsg); err == nil {
				if s := strings.TrimSpace(string(raw)); s != "" {
					text = s
				}
			}
			if text == "" {
				if errMsg != "" {
					return nil, fmt.Errorf("codex CLI 轮次失败: %s", truncate(errMsg, 2000))
				}
				return nil, fmt.Errorf("codex CLI 未输出答复; stderr: %s", stderr)
			}
			return &ai.TurnResult{Text: text, EngineSession: threadID, Usage: usage}, nil
		},
	}, nil
}
