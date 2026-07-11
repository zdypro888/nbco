package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHub 模拟中枢：/api/worker/llm 按脚本逐次返回消息，progress/submit 落记录。
type fakeHub struct {
	mu       sync.Mutex
	script   []chatMessage // 每次 LLM 调用弹出一条
	llmCalls int
	progress []string
	summary  string
	lessons  string
	question string
	failure  string
}

func (h *fakeHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/llm", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.llmCalls >= len(h.script) {
			t.Errorf("LLM 调用超出脚本（第 %d 次）", h.llmCalls+1)
			http.Error(w, "script exhausted", http.StatusInternalServerError)
			return
		}
		msg := h.script[h.llmCalls]
		h.llmCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
		})
	})
	mux.HandleFunc("POST /api/worker/progress", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.progress = append(h.progress, req.Content)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/worker/submit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Summary string `json:"summary"`
			Lessons string `json:"lessons"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.summary, h.lessons = req.Summary, req.Lessons
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/worker/request-input", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.question = req.Content
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/worker/fail", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.failure = req.Error
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func TestExecuteBuiltinCanRequestInputWithoutSubmitting(t *testing.T) {
	hub := &fakeHub{script: []chatMessage{
		callTool("c1", "request_input", `{"question":"请提供仓库 URL 和目标分支"}`),
	}}
	srv := httptest.NewServer(hub.handler(t))
	defer srv.Close()

	w := newWorker(Config{Server: srv.URL, Token: "t", Engine: engineBuiltin})
	task := &Task{ID: 11, ClaimID: "claim11", Title: "初始化仓库"}
	w.executeBuiltin(context.Background(), context.Background(), task, nil, nil, t.TempDir())

	if hub.question != "请提供仓库 URL 和目标分支" {
		t.Fatalf("question = %q", hub.question)
	}
	if hub.summary != "" {
		t.Fatalf("waiting task must not be submitted: %q", hub.summary)
	}
}

func callTool(id, name, args string) chatMessage {
	return chatMessage{Role: "assistant", ToolCalls: []toolCall{
		{ID: id, Type: "function", Function: toolCallFunc{Name: name, Arguments: args}},
	}}
}

// 内置智能体端到端：执行命令 → task_done → 提交验收。
func TestExecuteBuiltinRunsCommandAndSubmits(t *testing.T) {
	hub := &fakeHub{script: []chatMessage{
		callTool("c1", "run_command", `{"command":"echo builtin-agent-ok"}`),
		callTool("c2", "task_done", `{"summary":"已完成 echo 验证","lessons":"builtin 冒烟经验"}`),
	}}
	srv := httptest.NewServer(hub.handler(t))
	defer srv.Close()

	w := newWorker(Config{Server: srv.URL, Token: "t", Engine: engineBuiltin, WorkerName: "小码"})
	task := &Task{ID: 7, ClaimID: "claim7", Title: "冒烟测试", Description: "跑一条命令"}
	dir := t.TempDir()
	w.executeBuiltin(context.Background(), context.Background(), task, nil, nil, dir)

	if hub.llmCalls != 2 {
		t.Fatalf("应恰好两次模型调用, got %d", hub.llmCalls)
	}
	if hub.summary != "已完成 echo 验证" || hub.lessons != "builtin 冒烟经验" {
		t.Fatalf("提交内容不对: summary=%q lessons=%q", hub.summary, hub.lessons)
	}
	joined := strings.Join(hub.progress, "\n---\n")
	if !strings.Contains(joined, "echo builtin-agent-ok") {
		t.Errorf("进度里应有执行的命令，got:\n%s", joined)
	}
	if !strings.Contains(joined, "builtin-agent-ok") {
		t.Errorf("进度里应有命令输出，got:\n%s", joined)
	}
}

// 模型光说不练：提醒 maxNudges 次后按失败退避，不把未验证发言当完成提交。
func TestExecuteBuiltinNoToolFallback(t *testing.T) {
	talk := chatMessage{Role: "assistant", Content: "我认为任务已经完成了"}
	script := make([]chatMessage, 0, maxNudges+1)
	for i := 0; i <= maxNudges; i++ {
		script = append(script, talk)
	}
	hub := &fakeHub{script: script}
	srv := httptest.NewServer(hub.handler(t))
	defer srv.Close()

	w := newWorker(Config{Server: srv.URL, Token: "t", Engine: engineBuiltin})
	task := &Task{ID: 8, ClaimID: "claim8", Title: "空谈任务"}
	w.executeBuiltin(context.Background(), context.Background(), task, nil, nil, t.TempDir())

	if hub.llmCalls != maxNudges+1 {
		t.Fatalf("应在 %d 次后收尾, got %d", maxNudges+1, hub.llmCalls)
	}
	if hub.summary != "" {
		t.Fatalf("未验证任务不应提交完成, got %q", hub.summary)
	}
	if !strings.Contains(hub.failure, "未调用执行或完成工具") {
		t.Fatalf("应进入失败退避, got %q", hub.failure)
	}
}

// 命令输出会作为工具结果喂回模型（第二轮请求里带 tool 消息）。
func TestAgentToolResultFedBack(t *testing.T) {
	var secondReq []byte
	hub := &fakeHub{script: []chatMessage{
		callTool("c1", "run_command", `{"command":"echo feedback-check"}`),
		callTool("c2", "task_done", `{"summary":"ok"}`),
	}}
	inner := hub.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/llm" {
			hub.mu.Lock()
			n := hub.llmCalls
			hub.mu.Unlock()
			if n == 1 { // 第二次调用
				buf := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(buf)
				secondReq = buf
				r.Body = http.NoBody
				w.Header().Set("Content-Type", "application/json")
				hub.mu.Lock()
				msg := hub.script[hub.llmCalls]
				hub.llmCalls++
				hub.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg}}})
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	w := newWorker(Config{Server: srv.URL, Token: "t", Engine: engineBuiltin})
	task := &Task{ID: 9, ClaimID: "claim9", Title: "回喂测试"}
	w.executeBuiltin(context.Background(), context.Background(), task, nil, nil, t.TempDir())

	if !strings.Contains(string(secondReq), "feedback-check") {
		t.Fatalf("第二轮请求应包含上一条命令的输出, got: %s", string(secondReq))
	}
	if !strings.Contains(string(secondReq), `"tool_call_id":"c1"`) {
		t.Fatalf("第二轮请求应带 tool_call_id, got: %s", string(secondReq))
	}
}

func TestClipTailAndHead(t *testing.T) {
	long := strings.Repeat("界", 100)
	out := clipTail(long, 30)
	if !strings.HasPrefix(out, "[前序输出已截断]") || strings.Contains(out, "�") {
		t.Errorf("clipTail 应截断且不切坏多字节: %q", out)
	}
	if clipHead("短文本", 10) != "短文本" {
		t.Error("clipHead 不应截断短文本")
	}
	if h := clipHead(long, 10); len([]rune(h)) != 11 || !strings.HasSuffix(h, "…") {
		t.Errorf("clipHead 应按字符截断: %q", h)
	}
}

// 长任务对话超预算时，压缩早期工具输出、保留最近几条完整。
func TestCompactTranscript(t *testing.T) {
	big := strings.Repeat("x", 10<<10)
	msgs := []chatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "task"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs, chatMessage{Role: "assistant"}, chatMessage{Role: "tool", ToolCallID: "c", Content: big})
	}
	compactTranscript(msgs)
	var compacted, intact int
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, "[早期输出已压缩省略]") {
			compacted++
		} else if m.Content == big {
			intact++
		}
	}
	if intact != agentKeepRecentTools {
		t.Fatalf("应保留最近 %d 条完整输出, got %d", agentKeepRecentTools, intact)
	}
	if compacted != 8-agentKeepRecentTools {
		t.Fatalf("早期输出应全部压缩, got %d", compacted)
	}
	if msgs[0].Content != "sys" || msgs[1].Content != "task" {
		t.Fatal("system 与任务说明不应被压缩")
	}
	// 预算内的小对话不动。
	small := []chatMessage{{Role: "system", Content: "s"}, {Role: "tool", Content: "短输出"}}
	compactTranscript(small)
	if small[1].Content != "短输出" {
		t.Fatal("预算内不应压缩")
	}
}

// 模型调用瞬时失败自动重试，恢复后任务照常完成。
func TestLLMRetryRecovers(t *testing.T) {
	oldBackoff := agentRetryBackoff
	agentRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { agentRetryBackoff = oldBackoff }()

	fails := 2
	hub := &fakeHub{script: []chatMessage{
		callTool("c1", "task_done", `{"summary":"重试后完成"}`),
	}}
	inner := hub.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/llm" && fails > 0 {
			fails--
			http.Error(w, "upstream flake", http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	w := newWorker(Config{Server: srv.URL, Token: "t", Engine: engineBuiltin})
	task := &Task{ID: 10, ClaimID: "claim10", Title: "重试任务"}
	w.executeBuiltin(context.Background(), context.Background(), task, nil, nil, t.TempDir())

	if fails != 0 {
		t.Fatalf("应消耗完全部注入的失败, 剩 %d", fails)
	}
	if hub.summary != "重试后完成" {
		t.Fatalf("重试恢复后应正常提交, got %q", hub.summary)
	}
}
