package agentcli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zdypro888/nbco/internal/ai"
)

// ClaudeDriver 驱动 claude CLI：
//
//	claude -p --output-format stream-json --resume <会话>
//
// 内置工具全禁用（--tools ""），system prompt 走 --system-prompt，
// MCP 配置内联传入且 --strict-mcp-config 屏蔽用户自己的 MCP 配置。
type ClaudeDriver struct {
	Cmd   string // 可执行文件，默认 claude
	Model string // 可选
}

// Name 实现 Driver。
func (d *ClaudeDriver) Name() string { return "claudecli" }

// Plan 实现 Driver。
func (d *ClaudeDriver) Plan(req *ai.TurnRequest, mcpURL string) (*Plan, error) {
	dir, err := workDir("claude")
	if err != nil {
		return nil, err
	}
	mcpCfg, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"nbco": map[string]any{"type": "http", "url": mcpURL},
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
	if d.Model != "" {
		args = append(args, "--model", d.Model)
	}
	if req.EngineSession != "" {
		args = append(args, "--resume", req.EngineSession)
	}

	var res *ai.TurnResult
	var turnErr error
	return &Plan{
		Bin:  d.Cmd,
		Args: args,
		Dir:  dir,
		Consume: func(line []byte) {
			var ev struct {
				Type      string `json:"type"`
				Subtype   string `json:"subtype"`
				SessionID string `json:"session_id"`
				IsError   bool   `json:"is_error"`
				Result    string `json:"result"`
				Usage     *struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
				// init 事件：MCP 连接状态与可用工具（诊断关键）。
				MCPServers []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"mcp_servers"`
				Tools []string `json:"tools"`
			}
			if err := json.Unmarshal(line, &ev); err != nil {
				return // 容忍非 JSON 行
			}
			if ev.Type == "system" && ev.Subtype == "init" {
				for _, srv := range ev.MCPServers {
					if srv.Status != "connected" {
						slog.Warn("claude CLI MCP 服务未连接", "server", srv.Name, "status", srv.Status)
					} else {
						slog.Debug("claude CLI MCP 已连接", "server", srv.Name, "tools_total", len(ev.Tools))
					}
				}
				return
			}
			if ev.Type != "result" {
				return // 忽略其余中间事件
			}
			if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
				turnErr = fmt.Errorf("claude CLI 轮次失败(%s): %s", ev.Subtype, truncate(ev.Result, 2000))
				return
			}
			res = &ai.TurnResult{
				Text:          strings.TrimSpace(ev.Result),
				EngineSession: ev.SessionID,
			}
			if ev.Usage != nil {
				res.Usage = ai.Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
			}
		},
		Finish: func(waitErr error, stderr string) (*ai.TurnResult, error) {
			if turnErr != nil {
				return nil, turnErr
			}
			if res == nil {
				if waitErr != nil {
					return nil, fmt.Errorf("claude CLI 失败: %w; stderr: %s", waitErr, stderr)
				}
				return nil, fmt.Errorf("claude CLI 未输出 result 事件; stderr: %s", stderr)
			}
			return res, nil
		},
	}, nil
}
