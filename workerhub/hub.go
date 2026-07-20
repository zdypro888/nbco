// Package workerhub 维护 worker client 的实时连接（WebSocket 由网关层实现，
// 本包不依赖任何网络库）。
//
// 通道设计原则：数据库是唯一任务队列——派活=建任务、领活=原子认领，离线不丢活；
// 实时连接只做三件事：唤醒（新活来了立刻领，免等轮询）、取消（终止正在执行的
// 任务）、在线感知（连接在=真在线）。进程无状态原则不破坏：重启后 worker 重连
// 即恢复，无需持久化连接状态。
package workerhub

import "sync"

// 服务端 → worker 的消息类型。
const (
	MsgWake   = "wake"   // 有新任务，去认领
	MsgCancel = "cancel" // 取消指定任务（若正在执行则终止）
	MsgPong   = "pong"   // 应答 worker 的心跳
)

// Msg 实时通道消息（双向共用同一信封）。
type Msg struct {
	Type   string `json:"type"`
	RunID  int64  `json:"run_id,omitempty"`
	TaskID int64  `json:"task_id,omitempty"` // one-version worker compatibility
}

// Conn 一条已认证的 worker 连接，由网关层实现；Send 必须并发安全。
type Conn interface {
	Send(m Msg) error
}

// Hub 注册表：worker 用户 ID → 活跃连接（每个 worker 至多一条，新连顶旧连）。
type Hub struct {
	mu    sync.Mutex
	conns map[int64]Conn
}

func New() *Hub {
	return &Hub{conns: map[int64]Conn{}}
}

// Attach 注册连接，返回被顶掉的旧连接（若有，调用方负责关闭它）。
func (h *Hub) Attach(workerID int64, c Conn) (old Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old = h.conns[workerID]
	h.conns[workerID] = c
	return old
}

// Detach 解除连接；仅当当前登记的就是 c 时才移除（防止旧连接晚到的清理
// 把新连接顶掉）。
func (h *Hub) Detach(workerID int64, c Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[workerID] == c {
		delete(h.conns, workerID)
	}
}

// Online worker 是否有活跃实时连接。
func (h *Hub) Online(workerID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[workerID] != nil
}

// Wake 通知 worker 有新任务可领。尽力而为：不在线或发送失败都静默——
// 任务在库里，轮询兜底会拿到。
func (h *Hub) Wake(workerID int64) {
	h.send(workerID, Msg{Type: MsgWake})
}

// Cancel 通知 worker 终止某次执行。同样尽力而为。
func (h *Hub) Cancel(workerID, runID int64) {
	h.send(workerID, Msg{Type: MsgCancel, RunID: runID, TaskID: runID})
}

func (h *Hub) send(workerID int64, m Msg) {
	h.mu.Lock()
	c := h.conns[workerID]
	h.mu.Unlock()
	if c != nil {
		_ = c.Send(m)
	}
}
