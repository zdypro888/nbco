package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHub 模拟中枢：/api/worker/llm 按脚本逐次返回消息，progress/submit 落记录。
type fakeHub struct {
	mu       sync.Mutex
	script   []chatMessage // 每次 LLM 调用弹出一条
	llmCalls int
	progress []string
	summary  string
	lessons  string
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
	return mux
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

// 模型光说不练：提醒 maxNudges 次后把最后发言当总结提交，不死循环。
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
	if !strings.Contains(hub.summary, "我认为任务已经完成了") {
		t.Fatalf("应把最后发言当总结提交, got %q", hub.summary)
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
