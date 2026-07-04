package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/zdypro888/nbco/workerhub"
)

func TestWSURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:8900":  "ws://127.0.0.1:8900",
		"https://nb.example.com": "wss://nb.example.com",
		"127.0.0.1:8900":         "ws://127.0.0.1:8900",
	} {
		if got := newWSLink(base, "t").wsURL(); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", base, got, want)
		}
	}
}

// 假服务端推 wake + cancel，验证 link 分发到对应通道。
func TestWSLinkDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, workerhub.Msg{Type: workerhub.MsgWake})
		_ = wsjson.Write(ctx, c, workerhub.Msg{Type: workerhub.MsgCancel, TaskID: 7})
		// 挂住连接消费上行（ping），直到客户端断开。
		for {
			var m workerhub.Msg
			if wsjson.Read(ctx, c, &m) != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newWSLink(srv.URL, "tok")
	go l.run(ctx)

	select {
	case <-l.wake:
	case <-time.After(5 * time.Second):
		t.Fatal("未收到唤醒")
	}
	select {
	case id := <-l.cancel:
		if id != 7 {
			t.Fatalf("取消任务号 = %d", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到取消")
	}
}

// 取消当前任务：registerRun 后 cancelRun 语义。
func TestWorkerCancelCurrentRun(t *testing.T) {
	w := &Worker{link: newWSLink("http://x", "t")}
	runCtx, cancel := context.WithCancel(context.Background())
	w.registerRun(42, cancel)

	// 模拟 watchCancel 命中当前任务。
	w.mu.Lock()
	if w.curTask == 42 && w.curCancel != nil {
		w.curKilled = true
		w.curCancel()
	}
	w.mu.Unlock()

	select {
	case <-runCtx.Done():
	default:
		t.Fatal("执行上下文应已被取消")
	}
	if !w.unregisterRun() {
		t.Fatal("unregisterRun 应报告任务被取消")
	}
	if w.unregisterRun() {
		t.Fatal("二次调用应幂等返回 false")
	}
}
