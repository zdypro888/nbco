package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type cliInvocation struct {
	Args      []string
	ResumeRef string
}

var uuidSessionRefRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// cliInvocationFor returns the PTY command line for the configured engine. The
// worker still starts a short-lived interactive process per task; continuity is
// provided by the engine's native session resume when nbco has a safe ref.
func (w *Worker) cliInvocationFor(session SessionInfo, dir string) cliInvocation {
	base := w.cliArgs()
	inv := cliInvocation{Args: append([]string(nil), base...)}
	ref := cleanEngineSessionRef(session.EngineSessionRef)
	if ref == "" || len(w.cfg.Args) > 0 {
		return inv
	}
	// A native session owns its original CWD. Resuming it after a workspace
	// remap can make the CLI edit the wrong checkout or carry unrelated context.
	if canonicalDir(session.Workdir) == "" || canonicalDir(session.Workdir) != canonicalDir(dir) {
		return inv
	}
	switch strings.ToLower(strings.TrimSpace(w.cfg.Engine)) {
	case "codex":
		inv.Args = append([]string{"resume"}, base...)
		inv.Args = append(inv.Args, ref)
		inv.ResumeRef = ref
	case "claude":
		inv.Args = append(base, "--resume", ref)
		inv.ResumeRef = ref
	}
	return inv
}

func cleanEngineSessionRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if !uuidSessionRefRe.MatchString(ref) {
		return ""
	}
	return strings.ToLower(ref)
}

func (w *Worker) detectEngineSessionRef(dir string, since time.Time) string {
	ref, err := latestEngineSessionRef(w.cfg.Engine, dir, since)
	if err != nil {
		log.Printf("探测 %s 原生会话失败（不影响提交）: %v", w.cfg.Engine, err)
		return ""
	}
	return ref
}

func latestEngineSessionRef(engine, dir string, since time.Time) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "codex":
		return latestJSONLSessionRef(filepath.Join(home, ".codex", "sessions"), dir, since, parseCodexSessionLine)
	case "claude":
		return latestJSONLSessionRef(filepath.Join(home, ".claude", "projects"), dir, since, parseClaudeSessionLine)
	default:
		return "", nil
	}
}

type sessionMeta struct {
	ID        string
	CWD       string
	Timestamp time.Time
}

type sessionLineParser func([]byte) sessionMeta

func latestJSONLSessionRef(root, dir string, since time.Time, parse sessionLineParser) (string, error) {
	dir = canonicalDir(dir)
	if dir == "" {
		return "", nil
	}
	var best sessionMeta
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !since.IsZero() && info.ModTime().Before(since.Add(-2*time.Second)) {
			return nil
		}
		meta := readSessionMeta(path, parse)
		if meta.ID == "" || canonicalDir(meta.CWD) != dir {
			return nil
		}
		if meta.Timestamp.IsZero() {
			meta.Timestamp = info.ModTime()
		}
		if !since.IsZero() && meta.Timestamp.Before(since.Add(-2*time.Second)) && info.ModTime().Before(since.Add(-2*time.Second)) {
			return nil
		}
		if best.ID == "" || meta.Timestamp.After(best.Timestamp) || info.ModTime().After(best.Timestamp) {
			best = meta
			if info.ModTime().After(best.Timestamp) {
				best.Timestamp = info.ModTime()
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return best.ID, nil
}

func readSessionMeta(path string, parse sessionLineParser) sessionMeta {
	f, err := os.Open(path)
	if err != nil {
		return sessionMeta{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out sessionMeta
	for i := 0; i < 80 && sc.Scan(); i++ {
		meta := parse(sc.Bytes())
		if meta.ID != "" {
			out.ID = meta.ID
		}
		if meta.CWD != "" {
			out.CWD = meta.CWD
		}
		if !meta.Timestamp.IsZero() {
			out.Timestamp = meta.Timestamp
		}
		if out.ID != "" && out.CWD != "" {
			return out
		}
	}
	return out
}

func parseCodexSessionLine(line []byte) sessionMeta {
	var raw struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			SessionID string `json:"session_id"`
			ID        string `json:"id"`
			CWD       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return sessionMeta{}
	}
	if raw.Type != "" && raw.Type != "session_meta" {
		return sessionMeta{}
	}
	id := cleanEngineSessionRef(raw.Payload.SessionID)
	if id == "" {
		id = cleanEngineSessionRef(raw.Payload.ID)
	}
	return sessionMeta{ID: id, CWD: raw.Payload.CWD, Timestamp: parseSessionTime(firstNonEmpty(raw.Payload.Timestamp, raw.Timestamp))}
}

func parseClaudeSessionLine(line []byte) sessionMeta {
	var raw struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return sessionMeta{}
	}
	return sessionMeta{ID: cleanEngineSessionRef(raw.SessionID), CWD: raw.CWD, Timestamp: parseSessionTime(raw.Timestamp)}
}

func parseSessionTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

func canonicalDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
