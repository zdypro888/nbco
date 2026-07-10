package keylock

import (
	"sync"
	"testing"
)

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
