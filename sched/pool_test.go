package sched

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 并发上限：任意时刻在跑的任务数不超过 N。
func TestPoolBoundsConcurrency(t *testing.T) {
	const n = 3
	p := newPool(n)
	var cur, peak int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		p.submit(context.Background(), func() {
			defer wg.Done()
			c := atomic.AddInt64(&cur, 1)
			mu.Lock()
			if c > peak {
				peak = c
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt64(&cur, -1)
		})
	}
	wg.Wait()
	if peak > n {
		t.Fatalf("并发峰值 %d 超过上限 %d", peak, n)
	}
	if peak < 2 {
		t.Fatalf("并发峰值 %d，未真正并发", peak)
	}
}

// submit 对调用方非阻塞：即便任务很慢，提交本身立即返回。
func TestPoolSubmitNonBlocking(t *testing.T) {
	p := newPool(1)
	blocked := make(chan struct{})
	p.submit(context.Background(), func() { <-blocked }) // 占满唯一槽位
	done := make(chan struct{})
	go func() {
		start := time.Now()
		p.submit(context.Background(), func() {}) // 槽位占满时仍应立即返回
		if time.Since(start) > 200*time.Millisecond {
			t.Errorf("submit 阻塞了")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submit 未及时返回")
	}
	close(blocked)
}

// 全部提交的任务最终都会执行。
func TestPoolRunsAll(t *testing.T) {
	p := newPool(4)
	var ran int64
	for i := 0; i < 50; i++ {
		p.submit(context.Background(), func() { atomic.AddInt64(&ran, 1) })
	}
	p.wait()
	if ran != 50 {
		t.Fatalf("执行了 %d 个，应为 50", ran)
	}
}

func TestPoolRecoversPanic(t *testing.T) {
	p := newPool(1)
	var ran int64
	p.submit(context.Background(), func() { panic("boom") })
	p.submit(context.Background(), func() { atomic.AddInt64(&ran, 1) })
	p.wait()
	if ran != 1 {
		t.Fatalf("panic 后后续任务未执行，ran=%d", ran)
	}
}

// ctx 已取消时，未开始的任务不执行。
func TestPoolRespectsCancel(t *testing.T) {
	p := newPool(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消
	var ran int64
	for i := 0; i < 10; i++ {
		p.submit(ctx, func() { atomic.AddInt64(&ran, 1) })
	}
	p.wait()
	if ran != 0 {
		t.Fatalf("取消后仍执行了 %d 个", ran)
	}
}
