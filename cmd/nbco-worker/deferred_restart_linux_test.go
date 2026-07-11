//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestScheduleDeferredWorkerRestartAfterSubmit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "restart-required")
	current := filepath.Join(t.TempDir(), "nbco-worker")
	backup := filepath.Join(t.TempDir(), "nbco-worker.bak")
	t.Setenv(workerRestartMarkerEnv, marker)
	if err := os.WriteFile(backup, []byte("old worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "nbco-worker\n" + strconv.Itoa(os.Getpid()) + "\n" + current + "\n" + backup + "\n"
	if err := os.WriteFile(marker, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := runDeferredSystemdRestart
	t.Cleanup(func() { runDeferredSystemdRestart = previous })
	var got deferredWorkerRestart
	runDeferredSystemdRestart = func(_ string, restart deferredWorkerRestart) error {
		got = restart
		return nil
	}

	scheduled, err := scheduleDeferredWorkerRestart()
	if err != nil || !scheduled {
		t.Fatalf("schedule = %v, %v", scheduled, err)
	}
	if got.service != "nbco-worker" || got.currentBinary != current || got.backupBinary != backup {
		t.Fatalf("restart = %#v", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker should be removed after scheduling: %v", err)
	}
}

func TestScheduleDeferredWorkerRestartClearsFulfilledMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "restart-required")
	current := filepath.Join(t.TempDir(), "nbco-worker")
	backup := filepath.Join(t.TempDir(), "nbco-worker.bak")
	t.Setenv(workerRestartMarkerEnv, marker)
	if err := os.WriteFile(backup, []byte("old worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("nbco-worker\n999999999\n"+current+"\n"+backup+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := runDeferredSystemdRestart
	t.Cleanup(func() { runDeferredSystemdRestart = previous })
	runDeferredSystemdRestart = func(_ string, _ deferredWorkerRestart) error {
		t.Fatal("fulfilled marker must not schedule another restart")
		return nil
	}

	scheduled, err := scheduleDeferredWorkerRestart()
	if err != nil || scheduled {
		t.Fatalf("schedule = %v, %v", scheduled, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fulfilled marker should be removed: %v", err)
	}
}
