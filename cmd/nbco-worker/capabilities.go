package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func collectCapabilities(cfg Config) CapabilityReport {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	engine := strings.TrimSpace(cfg.Engine)
	bin := strings.TrimSpace(cfg.Bin)
	if bin == "" {
		bin = engine
	}
	caps := []string{"shell", "files", "text", "run-protocol-v2"}
	switch strings.ToLower(engine) {
	case "claude", "codex":
		caps = append(caps, "code", "repo", "long-context", "interactive-pty")
	case engineBuiltin:
		caps = append(caps, "commands", "light-agent")
	default:
		caps = append(caps, "interactive-pty", "custom-engine")
	}
	if commandExists("python3") || commandExists("python") {
		caps = append(caps, "python")
	}
	if commandExists("go") {
		caps = append(caps, "go")
	}
	if commandExists("git") {
		caps = append(caps, "git")
	}
	if commandExists("pdftotext") || commandExists("python3") {
		caps = append(caps, "pdf")
	}
	if commandExists("python3") {
		caps = append(caps, "xlsx", "csv", "images")
	}
	return CapabilityReport{
		Engine: engine, CLIName: bin, CLIVersion: cliVersion(bin),
		OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host, Workdir: home,
		Capabilities: dedupeStrings(caps),
		Metadata: map[string]any{
			"session_workspaces": cfg.SessionWorkspaces,
			"busy_pattern":       cfg.BusyPattern,
			"args_count":         len(cfg.Args),
			"model_source":       effectiveModelSource(cfg),
		},
	}
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func cliVersion(bin string) string {
	if strings.TrimSpace(bin) == "" || !commandExists(bin) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return clipHead(strings.TrimSpace(out.String()), 240)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
