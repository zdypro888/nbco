package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/nbco/workerproto"
)

// 内置智能体（engine=builtin）：工作机上没有 claude/codex 时，worker 自己就是
// 智能体——中枢模型当大脑（经 /api/worker/llm 透传管道，API key 不出中枢），
// 本机 shell 当手脚。能力弱于 claude/codex，但「能执行命令」已足以完成大量
// 运维/脚本/数据/构建类任务。与 PTY 铁律无关：这里不驱动任何 AI CLI。
const engineBuiltin = "builtin"

const (
	agentMaxSteps      = 40               // 单任务的模型步数上限（防失控循环）
	agentCmdTimeout    = 10 * time.Minute // run_command 默认时限
	agentCmdTimeoutMax = 30 * time.Minute // run_command 可指定的最大时限
	agentToolOutLimit  = 12 << 10         // 单条命令输出喂回模型的截断（保尾部）
	agentThoughtLimit  = 2000             // 模型阶段性说明记进度时的截断
	agentNoToolTurns   = 3                // 连续只叙述、不执行时的卡死判定

	// agentTranscriptBudget 每轮发送给模型的对话正文字节预算：长任务的工具输出
	// 全量累积会撑爆（本地）模型上下文，超预算时压缩早期工具输出。
	agentTranscriptBudget = 48 << 10
	// agentKeepRecentTools 压缩时保留完整原文的最近工具输出条数
	// （模型决策主要依赖任务说明与近期结果，早期原文价值低）。
	agentKeepRecentTools = 4
	// agentLLMRetries 模型调用瞬时失败（网关抖动/上游超时）的重试次数。
	agentLLMRetries = 2
)

// agentRetryBackoff 重试退避间隔，依次使用。
var agentRetryBackoff = []time.Duration{3 * time.Second, 10 * time.Second}

// —— OpenAI 兼容消息结构（与中枢管道对话用）——

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// agentTools 智能体的全部能力面：执行命令、请求信息、收尾提交。刻意不给独立的读写文件
// 工具——cat/写重定向都是命令，能力面越小越好审计。
var agentTools = []map[string]any{
	{"type": "function", "function": map[string]any{
		"name":        "run_command",
		"description": "在当前主题 workspace 执行一条 shell/cmd 命令并返回退出码与输出（超长输出截断保尾部）。一次一条、小步执行，看输出确认后再走下一步。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"command":     map[string]any{"type": "string", "description": "要执行的命令"},
			"timeout_sec": map[string]any{"type": "integer", "description": "可选超时秒数，默认600，最大1800"},
		}, "required": []string{"command"}},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "request_input",
		"description": "只有任务确实缺少分配者才能提供的关键信息、无法继续时调用；任务会暂停，待对方补充后以同一主题上下文重新领取。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "一个具体、可直接回答的问题"},
		}, "required": []string{"question"}},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "task_done",
		"description": "任务全部完成且自我验证通过后调用：提交验收总结。交付文件必须已放入 " + taskArtifactRelDir() + "/ 目录（系统自动上传）。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"summary": map[string]any{"type": "string", "description": "一句话说明做了什么、结果如何"},
			"lessons": map[string]any{"type": "string", "description": "可复用的经验教训，没有就留空"},
		}, "required": []string{"summary"}},
	}},
}

func agentSystemPrompt(name string) string {
	return fmt.Sprintf("你是公司的 AI 员工「%s」，在一台工作机的主题 workspace 中独立完成任务。\n"+
		"你可以用 run_command 操作工作机；确实缺少外部关键信息时用 request_input 暂停并向分配者提问。\n"+
		"工作原则：\n"+
		"- 小步执行：一次一条命令，根据真实输出决定下一步，绝不臆造结果\n"+
		"- 先看后动：不熟悉的环境先用 ls/cat/--version 等命令摸清情况\n"+
		"- workspace 可能为空；任务给出仓库地址时可自行 clone，不要仅因目录为空就停下\n"+
		"- 自我验证：完成后用命令实际检查验收标准是否达成\n"+
		"- 交付文件放进 "+taskArtifactRelDir()+"/ 目录（提交时自动上传）\n"+
		"- 验证通过后调用 task_done 提交，不要空谈计划不动手", name)
}

// agentTaskText 智能体的任务输入：正文与 CLI 路径共用，收尾走 task_done 工具
// 而非屏幕哨兵。
func agentTaskText(task *Run, knowledge, history []string) string {
	return taskBrief(task, knowledge, history) +
		"\n请从摸清现状开始，用 run_command 逐步完成并验证；缺关键外部信息时 request_input，否则最后 task_done 提交。"
}

// executeBuiltin 内置智能体主循环：模型决策 → 本机执行 → 结果喂回，直到
// task_done 或达到步数上限。进度、产物上传、提交验收全部复用 CLI 路径的机制。
func (w *Worker) executeBuiltin(ctx, runCtx context.Context, task *Run, knowledge, history []string, dir string) {
	msgs := []chatMessage{
		{Role: "system", Content: agentSystemPrompt(w.cfg.WorkerName)},
		{Role: "user", Content: agentTaskText(task, knowledge, history)},
	}
	w.report(ctx, task.ID, task.ClaimID, "🤖 内置智能体开始执行（中枢模型驱动）")
	noTool := 0
	for step := 1; step <= agentMaxSteps; step++ {
		if w.killed() {
			w.report(ctx, task.ID, task.ClaimID, "⛔ 任务被服务端取消，已终止执行。")
			return
		}
		compactTranscript(msgs)
		msg, err := w.llmWithRetry(runCtx, msgs)
		if err != nil {
			if w.killed() {
				w.report(ctx, task.ID, task.ClaimID, "⛔ 任务被服务端取消，已终止执行。")
				return
			}
			w.failTask(ctx, task, "内置智能体模型调用失败: "+err.Error(), task.Session, dir)
			return
		}
		if len(msg.ToolCalls) == 0 {
			// 光说不练：说明记进度，提醒几次后把最后发言当总结提交。
			if t := strings.TrimSpace(msg.Content); t != "" {
				w.report(ctx, task.ID, task.ClaimID, "💭 "+clipHead(t, agentThoughtLimit))
			}
			noTool++
			if noTool >= agentNoToolTurns {
				w.failTask(ctx, task, "内置智能体多次未调用执行或完成工具，无法确认任务完成", task.Session, dir)
				return
			}
			msgs = append(msgs, msg, chatMessage{Role: "user",
				Content: "请继续动手：用 run_command 执行下一步；若已全部完成并验证，调用 task_done 提交。"})
			continue
		}
		noTool = 0
		msgs = append(msgs, msg)
		for _, tc := range msg.ToolCalls {
			switch tc.Function.Name {
			case "task_done":
				var args struct {
					Summary string `json:"summary"`
					Lessons string `json:"lessons"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				summary := strings.TrimSpace(args.Summary)
				if summary == "" {
					summary = "任务完成（智能体未提供总结）。"
				}
				w.submitAgent(ctx, runCtx, task, dir, summary, strings.TrimSpace(args.Lessons))
				return
			case "request_input":
				var args struct {
					Question string `json:"question"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				question := strings.TrimSpace(args.Question)
				if question == "" {
					msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: tc.ID, Content: "question 不能为空"})
					continue
				}
				if err := w.client.RequestInput(ctx, task.ID, task.ClaimID, question, task.Session, dir); err != nil {
					msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: tc.ID, Content: "请求补充信息失败：" + err.Error()})
					continue
				}
				log.Printf("任务 #%d 已暂停，等待分配者补充信息", task.ID)
				return
			case "run_command":
				result := w.agentRunCommand(ctx, runCtx, task, dir, tc.Function.Arguments)
				msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
			default:
				msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: tc.ID,
					Content: "未知工具 " + tc.Function.Name + "，可用工具：run_command、request_input、task_done。"})
			}
		}
	}
	w.failTask(ctx, task, fmt.Sprintf("内置智能体达到 %d 步上限仍未收尾", agentMaxSteps), task.Session, dir)
}

// llmWithRetry 模型调用带瞬时故障重试；任务被取消/杀死时立即放弃。
func (w *Worker) llmWithRetry(ctx context.Context, msgs []chatMessage) (chatMessage, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		msg, err := w.client.LLM(ctx, msgs, agentTools)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		if attempt >= agentLLMRetries || ctx.Err() != nil || w.killed() {
			return chatMessage{}, lastErr
		}
		backoff := agentRetryBackoff[len(agentRetryBackoff)-1]
		if attempt < len(agentRetryBackoff) {
			backoff = agentRetryBackoff[attempt]
		}
		log.Printf("模型调用失败（第 %d 次），%s 后重试: %v", attempt+1, backoff, err)
		select {
		case <-ctx.Done():
			return chatMessage{}, lastErr
		case <-time.After(backoff):
		}
	}
}

// compactTranscript 对话超出预算时，把早期工具输出原地压成摘要占位，
// 保留最近 agentKeepRecentTools 条完整输出。system 与任务说明永不压缩。
func compactTranscript(msgs []chatMessage) {
	total := 0
	for i := range msgs {
		total += len(msgs[i].Content)
	}
	if total <= agentTranscriptBudget {
		return
	}
	var toolIdx []int
	for i := range msgs {
		if msgs[i].Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= agentKeepRecentTools {
		return
	}
	for _, i := range toolIdx[:len(toolIdx)-agentKeepRecentTools] {
		if total <= agentTranscriptBudget {
			return
		}
		c := msgs[i].Content
		if len(c) <= 160 {
			continue // 已经很短，压了也省不了多少
		}
		msgs[i].Content = clipHead(c, 100) + "\n[早期输出已压缩省略]"
		total -= len(c) - len(msgs[i].Content)
	}
}

// agentRunCommand 执行模型请求的命令并把结果整理成工具答复（同时回传进度）。
func (w *Worker) agentRunCommand(ctx, runCtx context.Context, task *Run, dir, rawArgs string) string {
	var args struct {
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "参数解析失败：" + err.Error()
	}
	if strings.TrimSpace(args.Command) == "" {
		return "command 不能为空。"
	}
	timeout := agentCmdTimeout
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
		if timeout > agentCmdTimeoutMax {
			timeout = agentCmdTimeoutMax
		}
	}
	w.report(ctx, task.ID, task.ClaimID, "🖥 执行命令：\n"+args.Command)
	cctx, cancel := context.WithTimeout(runCtx, timeout)
	defer cancel()
	res, err := runCommandExec(cctx, dir, args.Command, nil)
	out := clipTail(res.Output, agentToolOutLimit)
	if tail := tailText(out, 30); tail != "" {
		w.report(ctx, task.ID, task.ClaimID, fmt.Sprintf("🖥 输出（exit=%d，尾部）：\n%s", res.ExitCode, tail))
	}
	reply := fmt.Sprintf("退出码：%d", res.ExitCode)
	if err != nil {
		reply += "；执行错误：" + err.Error()
	}
	if out == "" {
		return reply + "\n（无输出）"
	}
	return reply + "\n输出：\n" + out
}

// submitAgent 收尾：上传产物、拼报告、提交结果（与 CLI 路径同一套约定）。
func (w *Worker) submitAgent(ctx, runCtx context.Context, task *Run, dir, summary, lessons string) {
	summary = w.appendArtifactReport(runCtx, task, dir, summary)
	if w.killed() {
		log.Printf("执行 #%d 在上传产物时被取消", task.ID)
		return
	}
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, lessons, task.Session, dir,
		SubmissionResult{Outcome: workerproto.OutcomeSucceeded}); err != nil {
		log.Printf("提交任务 #%d 失败: %v", task.ID, err)
		w.failTask(ctx, task, "提交内置智能体任务结果失败: "+err.Error(), task.Session, dir)
		w.handoffDeferredRestart()
		return
	}
	log.Printf("任务 #%d 已提交结果（内置智能体）", task.ID)
	w.handoffDeferredRestart()
}

// clipTail 超限截断保尾部（命令输出的关键信息通常在末尾），对齐 UTF-8 边界。
func clipTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[len(s)-limit:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return "[前序输出已截断]\n" + cut
}

// clipHead 超限截断保头部（说明性文本的重点通常在开头），按字符数不切坏多字节。
func clipHead(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
