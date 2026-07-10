package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunBackfillLoopRepeatsRecoversAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackfillLoop(ctx, "test", time.Millisecond, func(context.Context) {
			n := calls.Add(1)
			if n == 1 {
				panic("first pass")
			}
			if n == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("回填循环未随 context 停止")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("回填次数 = %d, want 3", got)
	}
}
