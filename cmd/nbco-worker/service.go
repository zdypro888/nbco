package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func installService(args []string) {
	fs := flag.NewFlagSet("install-service", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	engine := fs.String("engine", "", "覆盖引擎：claude | codex")
	bin := fs.String("bin", "", "覆盖 CLI 可执行文件路径")
	name := fs.String("name", "", "服务名（同机多 worker 时建议指定）")
	_ = fs.Parse(args)
	path := configPath(*cfgFile)
	if err := installServiceForConfig(path, *engine, *bin, *name); err != nil {
		log.Fatalf("安装系统服务失败: %v", err)
	}
	fmt.Println("系统服务已安装并启动。")
}

func uninstallService(args []string) {
	fs := flag.NewFlagSet("uninstall-service", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	name := fs.String("name", "", "服务名")
	_ = fs.Parse(args)
	path := configPath(*cfgFile)
	if err := uninstallPlatformService(serviceName(path, *name)); err != nil {
		log.Fatalf("卸载系统服务失败: %v", err)
	}
	fmt.Println("系统服务已卸载。")
}

func serviceStatus(args []string) {
	fs := flag.NewFlagSet("service-status", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	name := fs.String("name", "", "服务名")
	_ = fs.Parse(args)
	path := configPath(*cfgFile)
	out, err := platformServiceStatus(serviceName(path, *name))
	if err != nil {
		log.Fatalf("查询系统服务失败: %v\n%s", err, out)
	}
	fmt.Print(out)
}

func installServiceForConfig(path, engine, bin, nameOverride string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(engine) != "" {
		cfg.Engine = strings.TrimSpace(engine)
	}
	if cfg.Engine == "" {
		cfg.Engine = "claude"
	}
	if strings.TrimSpace(bin) != "" {
		cfg.Bin = strings.TrimSpace(bin)
	}
	if cfg.Bin == "" {
		if found, err := exec.LookPath(cfg.Engine); err == nil {
			cfg.Bin = found
		}
	}
	if cfg.Bin != "" {
		if abs, err := filepath.Abs(cfg.Bin); err == nil {
			cfg.Bin = abs
		}
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	return installPlatformService(serviceName(path, nameOverride), exe, path)
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return cfg, nil
}

func serviceName(configFile, override string) string {
	if strings.TrimSpace(override) != "" {
		return cleanServiceName(override)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		def := filepath.Join(home, ".nbco-worker.json")
		if filepath.Clean(configFile) == filepath.Clean(def) {
			return "nbco-worker"
		}
	}
	base := strings.TrimSuffix(filepath.Base(configFile), filepath.Ext(configFile))
	if base == "" || base == "." {
		base = "worker"
	}
	return "nbco-worker-" + cleanServiceName(base)
}

func cleanServiceName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "nbco-worker"
	}
	return out
}

func installPlatformService(name, exe, cfgPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(name, exe, cfgPath)
	case "linux":
		return installSystemdUser(name, exe, cfgPath)
	case "windows":
		return installWindowsTask(name, exe, cfgPath)
	default:
		return fmt.Errorf("暂不支持在 %s 上自动安装服务；可手动常驻运行：%s run -config %s", runtime.GOOS, exe, cfgPath)
	}
}

func uninstallPlatformService(name string) error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchAgent(name)
	case "linux":
		return uninstallSystemdUser(name)
	case "windows":
		return uninstallWindowsTask(name)
	default:
		return fmt.Errorf("暂不支持在 %s 上自动卸载服务", runtime.GOOS)
	}
}

func platformServiceStatus(name string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return runOutput("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchLabel(name))
	case "linux":
		return runOutput("systemctl", "--user", "status", "--no-pager", systemdUnit(name))
	case "windows":
		return runOutput("schtasks", "/Query", "/TN", windowsTaskName(name), "/V", "/FO", "LIST")
	default:
		return "", fmt.Errorf("暂不支持在 %s 上查询服务", runtime.GOOS)
	}
}

func installLaunchAgent(name, exe, cfgPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := launchLabel(name)
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	plist := filepath.Join(dir, label+".plist")
	pathEnv := home + "/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Codex.app/Contents/Resources"
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>-config</string>
    <string>%s</string>
  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>%s</string>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(label), xmlEscape(exe), xmlEscape(cfgPath), xmlEscape(home), xmlEscape(home), xmlEscape(pathEnv),
		xmlEscape(filepath.Join(home, "Library", "Logs", name+".out.log")),
		xmlEscape(filepath.Join(home, "Library", "Logs", name+".err.log")))
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, plist).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallLaunchAgent(name string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", launchLabel(name)+".plist")
	_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), plist).Run()
	return os.RemoveAll(plist)
}

func launchLabel(name string) string { return "com.zdypro." + cleanServiceName(name) }

func installSystemdUser(name, exe, cfgPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := systemdUnit(name)
	path := filepath.Join(dir, unit)
	body := fmt.Sprintf(`[Unit]
Description=NBCO worker %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run -config %s
Restart=always
RestartSec=10
WorkingDirectory=%s
Environment=HOME=%s
Environment=PATH=%s/.local/bin:/usr/local/bin:/usr/bin:/bin

[Install]
WantedBy=default.target
`, name, systemdQuote(exe), systemdQuote(cfgPath), systemdQuote(home), systemdQuote(home), home)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallSystemdUser(name string) error {
	unit := systemdUnit(name)
	_, _ = exec.Command("systemctl", "--user", "disable", "--now", unit).CombinedOutput()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", unit))
	_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	return nil
}

func systemdUnit(name string) string { return cleanServiceName(name) + ".service" }

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	return `"` + s + `"`
}

func installWindowsTask(name, exe, cfgPath string) error {
	task := windowsTaskName(name)
	cmd := windowsQuote(exe) + " run -config " + windowsQuote(cfgPath)
	out, err := exec.Command("schtasks", "/Create", "/TN", task, "/SC", "ONLOGON", "/TR", cmd, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallWindowsTask(name string) error {
	out, err := exec.Command("schtasks", "/Delete", "/TN", windowsTaskName(name), "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks delete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func windowsTaskName(name string) string { return `\NBCO\` + cleanServiceName(name) }

func windowsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func xmlEscape(s string) string {
	return html.EscapeString(s)
}

func runOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
