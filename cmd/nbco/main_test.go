package main

import (
	"context"
	"runtime/debug"
	"sync"
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

func TestRunBackfillExclusiveUnlocksAfterPanic(t *testing.T) {
	var mu sync.Mutex
	func() {
		defer func() { _ = recover() }()
		runBackfillExclusive(&mu, func() { panic("test") })
	}()
	done := make(chan struct{})
	go func() {
		runBackfillExclusive(&mu, func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("exclusive backfill lock remained held after panic")
	}
}

func TestResolveVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "9fd96bde9e10ea4d12346703974bb11af8de3756"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	if got := resolveVersion("release-7", info); got != "release-7" {
		t.Fatalf("linked version = %q", got)
	}
	if got := resolveVersion("dev", info); got != "9fd96bde9e10-dirty" {
		t.Fatalf("VCS fallback = %q", got)
	}
	info.Settings = nil
	if got := resolveVersion("dev", info); got != "v1.2.3" {
		t.Fatalf("module fallback = %q", got)
	}
	if got := resolveVersion("dev", nil); got != "dev" {
		t.Fatalf("development fallback = %q", got)
	}
}
