//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandExecCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runCommandExec(ctx, dir, `sleep 60 & echo $! > child.pid; wait`, nil)
		done <- err
	}()

	pidPath := filepath.Join(dir, "child.pid")
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runCommandExec error = %v, want context canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not stop after cancellation")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived cancellation", childPID)
}
