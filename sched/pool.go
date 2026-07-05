package sched

import (
	"context"
	"log/slog"
	"sync"
)

// pool 是一个信号量限并发的异步执行器：把「跑一轮 AI + 推送」这类重活从
// 调度器 tick 的关键路径上挪走，既不阻塞 30 秒节拍（截止提醒等确定性任务照常
// 及时触发），又用固定并发上限护住后端模型网关（避免「全员问候」几百轮齐发）。
//
// 正确性依赖于各 pass 的「原子短租约 + 成功 ack」：待办先 claim，投递成功后才写
// sent 标记；进程崩溃或发送失败时租约过期后重试。
type pool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func newPool(n int) *pool {
	if n < 1 {
		n = 1
	}
	return &pool{sem: make(chan struct{}, n)}
}

// submit 在受限并发下异步执行 fn；对调用方非阻塞。ctx 取消时未开始的任务放弃。
func (p *pool) submit(ctx context.Context, fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-p.sem }()
		if ctx.Err() != nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Error("后台任务 panic 已恢复", "panic", r)
			}
		}()
		fn()
	}()
}

// wait 等待所有已派发任务结束（测试用；Run 不调用它以免关停被长 AI 轮次拖住）。
func (p *pool) wait() { p.wg.Wait() }
