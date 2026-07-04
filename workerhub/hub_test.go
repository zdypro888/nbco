package workerhub

import (
	"sync"
	"testing"
)

type fakeConn struct {
	mu   sync.Mutex
	msgs []Msg
}

func (f *fakeConn) Send(m Msg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakeConn) got() []Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Msg(nil), f.msgs...)
}

func TestHubWakeAndCancel(t *testing.T) {
	h := New()
	c := &fakeConn{}
	if old := h.Attach(7, c); old != nil {
		t.Fatal("首次注册不应有旧连接")
	}
	if !h.Online(7) {
		t.Fatal("注册后应在线")
	}
	h.Wake(7)
	h.Cancel(7, 42)
	msgs := c.got()
	if len(msgs) != 2 || msgs[0].Type != MsgWake || msgs[1].Type != MsgCancel || msgs[1].TaskID != 42 {
		t.Fatalf("消息不对: %+v", msgs)
	}
	// 不在线的 worker：静默无事。
	h.Wake(99)
	h.Cancel(99, 1)
}

func TestHubReplaceAndDetach(t *testing.T) {
	h := New()
	c1, c2 := &fakeConn{}, &fakeConn{}
	h.Attach(7, c1)
	if old := h.Attach(7, c2); old != c1 {
		t.Fatal("新连接应顶掉旧连接并返回旧连接")
	}
	// 旧连接晚到的 Detach 不应影响新连接。
	h.Detach(7, c1)
	if !h.Online(7) {
		t.Fatal("旧连接的清理不应把新连接顶下线")
	}
	h.Wake(7)
	if len(c1.got()) != 0 || len(c2.got()) != 1 {
		t.Fatal("消息应只发给新连接")
	}
	h.Detach(7, c2)
	if h.Online(7) {
		t.Fatal("Detach 后应离线")
	}
}
