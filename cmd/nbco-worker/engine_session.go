package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type cliInvocation struct {
	Args               []string
	Env                []string
	ResumeRef          string
	RuntimeFingerprint string
}

const (
	modelSourceCentral   = "central"
	modelSourceLocal     = "local"
	workerModelTokenEnv  = "NBCO_WORKER_MODEL_TOKEN"
	centralCodexProvider = "nbco_central"
)

var uuidSessionRefRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// cliInvocationFor returns the PTY command line for the configured engine. The
// worker still starts a short-lived interactive process per task; continuity is
// provided by the engine's native session resume when nbco has a safe ref.
func (w *Worker) cliInvocationFor(session SessionInfo, dir string, runtime *EngineRuntime) cliInvocation {
	base := w.cliArgsForRuntime(runtime)
	inv := cliInvocation{
		Args:               append([]string(nil), base...),
		RuntimeFingerprint: w.engineRuntimeFingerprint(dir, runtime),
	}
	if runtime != nil {
		inv.Env = []string{workerModelTokenEnv + "=" + w.cfg.Token}
	}
	ref := cleanEngineSessionRef(session.EngineSessionRef)
	if ref == "" || len(w.cfg.Args) > 0 ||
		strings.TrimSpace(session.EngineRuntimeFingerprint) == "" ||
		!strings.EqualFold(session.EngineRuntimeFingerprint, inv.RuntimeFingerprint) {
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
		inv.Args = append(append([]string(nil), base...), "--resume", ref)
		inv.ResumeRef = ref
	}
	return inv
}

func (w *Worker) cliArgsForRuntime(runtime *EngineRuntime) []string {
	base := w.cliArgs()
	if runtime == nil || !strings.EqualFold(strings.TrimSpace(w.cfg.Engine), "codex") || len(w.cfg.Args) > 0 {
		return base
	}
	providerPrefix := "model_providers." + centralCodexProvider + "."
	return append(base,
		"-c", "model="+strconv.Quote(strings.TrimSpace(runtime.Model)),
		"-c", "model_provider="+strconv.Quote(centralCodexProvider),
		"-c", providerPrefix+"name="+strconv.Quote("nbco central model"),
		"-c", providerPrefix+"base_url="+strconv.Quote(strings.TrimRight(runtime.BaseURL, "/")),
		"-c", providerPrefix+"env_key="+strconv.Quote(workerModelTokenEnv),
		"-c", providerPrefix+"wire_api="+strconv.Quote("responses"),
	)
}

func (w *Worker) usesCentralModelRuntime() bool {
	return effectiveModelSource(w.cfg) == modelSourceCentral
}

func effectiveModelSource(cfg Config) string {
	if !strings.EqualFold(strings.TrimSpace(cfg.Engine), "codex") || len(cfg.Args) > 0 {
		return modelSourceLocal
	}
	if strings.EqualFold(strings.TrimSpace(cfg.ModelSource), modelSourceLocal) {
		return modelSourceLocal
	}
	return modelSourceCentral
}

type runtimeFingerprintFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}

// engineRuntimeFingerprint identifies the local runtime assumptions captured
// by a native CLI session. It deliberately hashes, rather than uploads, config
// files and environment values. The server only needs an equality token to
// decide whether a stored native session is still safe to resume.
func (w *Worker) engineRuntimeFingerprint(dir string, runtime *EngineRuntime) string {
	engine := strings.ToLower(strings.TrimSpace(w.cfg.Engine))
	bin := strings.TrimSpace(w.cfg.Bin)
	if bin == "" {
		bin = engine
	}
	resolvedBin := bin
	if path, err := exec.LookPath(bin); err == nil {
		resolvedBin = canonicalDir(path)
	}

	files := make([]runtimeFingerprintFile, 0)
	for _, path := range w.engineRuntimeFiles(engine, dir) {
		files = append(files, w.fingerprintRuntimeFile(engine, path))
	}
	environment := w.engineRuntimeEnvironment(engine)
	payload := struct {
		Version     int                      `json:"version"`
		Engine      string                   `json:"engine"`
		Binary      string                   `json:"binary"`
		CLIVersion  string                   `json:"cli_version"`
		Args        []string                 `json:"args"`
		Files       []runtimeFingerprintFile `json:"files"`
		Environment []string                 `json:"environment"`
		Runtime     *EngineRuntime           `json:"runtime,omitempty"`
	}{
		Version: 1, Engine: engine, Binary: resolvedBin, CLIVersion: cliVersion(bin),
		Args: append([]string(nil), w.cliArgsForRuntime(runtime)...), Files: files, Environment: environment,
		Runtime: runtime,
	}
	raw, _ := json.Marshal(payload)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (w *Worker) fingerprintRuntimeFile(engine, path string) runtimeFingerprintFile {
	if engine == "codex" && w.isCodexConfigPath(path) {
		return fingerprintCodexConfig(path)
	}
	return fingerprintFile(path)
}

func (w *Worker) isCodexConfigPath(path string) bool {
	home, _ := os.UserHomeDir()
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	path = canonicalDir(path)
	if path == canonicalDir(filepath.Join(codexHome, "config.toml")) {
		return true
	}
	return strings.HasSuffix(filepath.ToSlash(path), "/.codex/config.toml")
}

// fingerprintCodexConfig excludes local UI bookkeeping and trust acknowledgements
// which Codex rewrites during normal startup. Model, provider, features, sandbox,
// MCP and every other runtime-affecting setting remain in the canonical digest.
// Without this semantic normalization, an unrelated TUI counter would rotate the
// native session on every task and defeat continuity.
func fingerprintCodexConfig(path string) runtimeFingerprintFile {
	path = canonicalDir(path)
	entry := runtimeFingerprintFile{Path: path, State: "missing"}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			entry.State = "unreadable"
		}
		return entry
	}
	var cfg map[string]any
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return fingerprintBytes(entry, raw, "present_raw")
	}
	delete(cfg, "projects")
	delete(cfg, "tui")
	canonical, err := json.Marshal(cfg)
	if err != nil {
		return fingerprintBytes(entry, raw, "present_raw")
	}
	return fingerprintBytes(entry, canonical, "present")
}

func fingerprintBytes(entry runtimeFingerprintFile, data []byte, state string) runtimeFingerprintFile {
	entry.State = state
	entry.Digest = fmt.Sprintf("%x", sha256.Sum256(data))
	return entry
}

func (w *Worker) engineRuntimeFiles(engine, dir string) []string {
	home, _ := os.UserHomeDir()
	paths := make([]string, 0, len(w.cfg.SessionRuntimeFiles)+8)
	switch engine {
	case "codex":
		codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		paths = append(paths, filepath.Join(codexHome, "config.toml"))
		paths = append(paths, ancestorRuntimeFiles(dir, ".codex/config.toml")...)
	case "claude":
		paths = append(paths,
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "settings.local.json"),
			filepath.Join(home, ".claude.json"),
		)
		paths = append(paths, ancestorRuntimeFiles(dir, ".claude/settings.json")...)
		paths = append(paths, ancestorRuntimeFiles(dir, ".claude/settings.local.json")...)
	}
	for _, path := range w.cfg.SessionRuntimeFiles {
		paths = append(paths, expandRuntimePath(path, home))
	}
	return dedupeSortedPaths(paths)
}

func (w *Worker) engineRuntimeEnvironment(engine string) []string {
	prefixes := []string(nil)
	switch engine {
	case "codex":
		prefixes = []string{"CODEX_", "OPENAI_"}
	case "claude":
		prefixes = []string{"ANTHROPIC_", "CLAUDE_"}
	}
	exact := make(map[string]bool, len(w.cfg.SessionRuntimeEnv))
	for _, name := range w.cfg.SessionRuntimeEnv {
		if name = strings.TrimSpace(name); name != "" {
			exact[name] = true
		}
	}
	values := make([]string, 0)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		matched := exact[name]
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				matched = true
				break
			}
		}
		if matched {
			values = append(values, item)
			delete(exact, name)
		}
	}
	for name := range exact {
		values = append(values, name+"=<unset>")
	}
	sort.Strings(values)
	return values
}

func fingerprintFile(path string) runtimeFingerprintFile {
	path = canonicalDir(path)
	entry := runtimeFingerprintFile{Path: path, State: "missing"}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			entry.State = "unreadable"
		}
		return entry
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		entry.State = "unreadable"
		return entry
	}
	entry.State = "present"
	entry.Digest = fmt.Sprintf("%x", h.Sum(nil))
	return entry
}

func ancestorRuntimeFiles(dir, relative string) []string {
	dir = canonicalDir(dir)
	if dir == "" {
		return nil
	}
	var paths []string
	for {
		paths = append(paths, filepath.Join(dir, filepath.FromSlash(relative)))
		parent := filepath.Dir(dir)
		if parent == dir {
			return paths
		}
		dir = parent
	}
}

func expandRuntimePath(path, home string) string {
	path = os.ExpandEnv(strings.TrimSpace(path))
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func dedupeSortedPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = canonicalDir(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
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
