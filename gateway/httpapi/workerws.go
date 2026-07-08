package httpapi

// worker 实时通道（WebSocket）。
// 设计：数据库是唯一任务队列，认领/进度/提交仍走原 HTTP 接口，重启无状态；
// WS 只做三件事——唤醒（新活秒领）、取消（终止在跑任务）、在线感知（连接即在线）。
// worker 离线丢的只是即时性，轮询兜底一切照旧。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/workerhub"
)

// wsReadTimeout 多久没收到任何上行（worker 每 30 秒 ping）判定连接已死。
const wsReadTimeout = 90 * time.Second

// wsConn workerhub.Conn 的 WebSocket 实现；Send 并发安全。
type wsConn struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (w *wsConn) Send(m workerhub.Msg) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, w.c, m)
}

// handleWorkerWS 升级为 WebSocket 并挂进 hub，读循环处理心跳直到断开。
func (s *Server) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	hub := s.deps.Workers
	if hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "实时通道未启用"})
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept 已写响应
	}
	conn := &wsConn{c: c}
	if old := hub.Attach(u.ID, conn); old != nil {
		if oc, ok := old.(*wsConn); ok {
			_ = oc.c.Close(websocket.StatusNormalClosure, "被同一 worker 的新连接顶替")
		}
	}
	ctx := r.Context()
	_ = s.store.WorkerHeartbeat(ctx, u.ID)
	slog.Info("worker 实时通道上线", "worker", u.ID)
	defer func() {
		hub.Detach(u.ID, conn)
		_ = c.CloseNow()
		slog.Info("worker 实时通道下线", "worker", u.ID)
		s.notifyWorkerOffline(u) // 持有在办任务时向监护人推"执行中离线"事件
	}()

	for {
		rctx, cancel := context.WithTimeout(ctx, wsReadTimeout)
		var m workerhub.Msg
		rerr := wsjson.Read(rctx, c, &m)
		cancel()
		if rerr != nil {
			return
		}
		if m.Type == "ping" {
			_ = s.store.WorkerHeartbeat(ctx, u.ID)
			_ = conn.Send(workerhub.Msg{Type: workerhub.MsgPong})
		}
	}
}

// notifyWorkerOffline worker 实时通道断开时，若该 worker 仍持有在办任务，向其
// 监护人推一个"执行中离线"事件，让 AI 决定是否通知相关人/改派——把"被动等 48h
// 催办"前置成"掉线即感知"。无在办任务则不打扰；重复离线由事件总线去重兜底。
func (s *Server) notifyWorkerOffline(u *store.User) {
	if s.bus == nil || u == nil || !u.IsWorker || u.OwnerID == nil || *u.OwnerID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tasks, err := s.store.TasksOfAssignee(ctx, u.ID, true)
	if err != nil || len(tasks) == 0 {
		return
	}
	s.bus.Emit("worker 执行中离线", *u.OwnerID,
		fmt.Sprintf("AI 员工「%s」实时通道断开，仍有 %d 个在办任务。请核实是否需要改派或通知相关人。",
			u.Name, len(tasks)))
}
