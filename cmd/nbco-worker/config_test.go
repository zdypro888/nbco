package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathOverrideAndEnv(t *testing.T) {
	t.Setenv(workerConfigEnv, "/tmp/env-worker.json")
	if got := configPath("/tmp/flag-worker.json"); got != "/tmp/flag-worker.json" {
		t.Fatalf("flag config path = %q", got)
	}
	if got := configPath(""); got != "/tmp/env-worker.json" {
		t.Fatalf("env config path = %q", got)
	}
}

func TestSaveConfigCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers", "front.json")
	cfg := Config{Server: "https://nbco.example.com", Token: "tok", WorkerID: 9, WorkerName: "front", Engine: "claude"}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config mode = %o, want 600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.WorkerID != cfg.WorkerID || got.WorkerName != cfg.WorkerName || got.Token != cfg.Token {
		t.Fatalf("config = %+v", got)
	}
}

func TestSaveConfigTightensExistingFilePermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows does not expose POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(path, Config{Server: "https://nbco.example.com", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
}

func TestServiceNameDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := serviceName(filepath.Join(home, ".nbco-worker.json"), ""); got != "nbco-worker" {
		t.Fatalf("default service name = %q", got)
	}
	got := serviceName(filepath.Join(home, ".config", "nbco", "workers", "Front End.json"), "")
	if got != "nbco-worker-front-end" {
		t.Fatalf("named config service = %q", got)
	}
	if got := serviceName("/x/y.json", "Reviewer #1"); got != "reviewer-1" {
		t.Fatalf("override service name = %q", got)
	}
}
