package keylock

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMapAcquireContextStopsWaiting(t *testing.T) {
	var locks Map[string]
	release := locks.Acquire("same")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.AcquireContext(ctx, "same"); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireContext error = %v", err)
	}
	if got := locks.Len(); got != 1 {
		t.Fatalf("cancelled waiter leaked a ref: len=%d", got)
	}
	release()
	if got := locks.Len(); got != 0 {
		t.Fatalf("released lock leaked: len=%d", got)
	}
}

func TestMapAcquireContextRejectsAlreadyCancelledFreeLock(t *testing.T) {
	var locks Map[string]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		if release, err := locks.AcquireContext(ctx, "free"); !errors.Is(err, context.Canceled) {
			if release != nil {
				release()
			}
			t.Fatalf("cancelled context acquired a free lock: %v", err)
		}
	}
	if locks.Len() != 0 {
		t.Fatalf("cancelled acquisitions leaked lock entries: %d", locks.Len())
	}
}

func TestMapSerializesSameKeyAndEvicts(t *testing.T) {
	var locks Map[string]
	release := locks.Acquire("same")
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		releaseSecond := locks.Acquire("same")
		close(acquired)
		releaseSecond()
	}()
	select {
	case <-acquired:
		t.Fatal("second acquire must wait")
	default:
	}
	release()
	<-done
	if got := locks.Len(); got != 0 {
		t.Fatalf("idle entries = %d, want 0", got)
	}
}

func TestMapAllowsDifferentKeys(t *testing.T) {
	var locks Map[int]
	releaseOne := locks.Acquire(1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		releaseTwo := locks.Acquire(2)
		releaseTwo()
	}()
	wg.Wait()
	releaseOne()
	if got := locks.Len(); got != 0 {
		t.Fatalf("idle entries = %d, want 0", got)
	}
}
