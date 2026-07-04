package main

// 实时通道（WebSocket）client 侧。纯增强件：唤醒=秒领任务、取消=终止在跑任务、
// 心跳=真实时在线。连不上完全不影响干活——任务队列在数据库里，轮询兜底。

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/zdypro888/nbco/workerhub"
)

const (
	wsPingInterval = 30 * time.Second
	wsIdleTimeout  = 90 * time.Second // 这么久没有任何下行（pong/wake）判定连接死
	wsBackoffMin   = 2 * time.Second
	wsBackoffMax   = 60 * time.Second
)

// wsLink 维护到服务端的实时通道，把下行消息翻译成两个事件通道。
type wsLink struct {
	base, token string
	wake        chan struct{} // 有新任务可领（合并信号，缓冲 1）
	cancel      chan int64    // 取消某任务
}

func newWSLink(base, token string) *wsLink {
	return &wsLink{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		wake:   make(chan struct{}, 1),
		cancel: make(chan int64, 4),
	}
}

// run 维持连接直到 ctx 取消：断线指数退避重连，稳定连接后退避归位。
func (l *wsLink) run(ctx context.Context) {
	backoff := wsBackoffMin
	for ctx.Err() == nil {
		start := time.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = wsBackoffMin // 刚才是条稳定连接，重置退避
		}
		log.Printf("实时通道断开: %v（%s 后重连；期间轮询兜底）", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > wsBackoffMax {
			backoff = wsBackoffMax
		}
	}
}

// session 一次连接的完整生命周期：拨号 → 心跳协程 → 读循环分发。
func (l *wsLink) session(ctx context.Context) error {
	url := l.wsURL() + "/api/worker/ws"
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + l.token}},
	})
	dcancel()
	if err != nil {
		return err
	}
	defer c.CloseNow()
	log.Printf("实时通道已连接：%s", url)

	stop := make(chan struct{})
	defer close(stop)
	go func() { // 心跳：服务端以此刷新在线状态并回 pong
		t := time.NewTicker(wsPingInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				werr := wsjson.Write(wctx, c, workerhub.Msg{Type: "ping"})
				wcancel()
				if werr != nil {
					return // 写失败连接已死，读循环随即报错退出
				}
			}
		}
	}()

	for {
		rctx, rcancel := context.WithTimeout(ctx, wsIdleTimeout)
		var m workerhub.Msg
		rerr := wsjson.Read(rctx, c, &m)
		rcancel()
		if rerr != nil {
			return rerr
		}
		switch m.Type {
		case workerhub.MsgWake:
			select {
			case l.wake <- struct{}{}:
			default: // 已有待处理的唤醒，合并
			}
		case workerhub.MsgCancel:
			select {
			case l.cancel <- m.TaskID:
			default:
			}
		}
	}
}

// wsURL http(s)://… → ws(s)://…
func (l *wsLink) wsURL() string {
	switch {
	case strings.HasPrefix(l.base, "https://"):
		return "wss://" + strings.TrimPrefix(l.base, "https://")
	case strings.HasPrefix(l.base, "http://"):
		return "ws://" + strings.TrimPrefix(l.base, "http://")
	default:
		return "ws://" + l.base
	}
}
