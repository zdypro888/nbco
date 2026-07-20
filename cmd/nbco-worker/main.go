// nbco-worker：装在工作机上的 AI 员工 client。
// 轮询 nbco 领取分配给自己的任务，用 PTY 驱动 claude/codex 交互式执行，
// 把终端输出实时回传作为进度，完成后提交验收。
// 机器上没有 claude/codex 时自动回退内置智能体（engine=builtin）：中枢模型
// 当大脑、本机 shell 当手脚，能力较弱但同样能领活干活。
//
//	nbco-worker bind [-config path] <server> <绑定码|token>   # 绑定，写本机配置
//	nbco-worker bootstrap [-config path] [-engine claude|codex|builtin] [-bin /path/to/cli] <server> <绑定码|token>
//	                                                       # 绑定并安装为系统服务
//	nbco-worker run [-config path]                      # 上线接活
//
// 凭据支持两种：create_worker 给出的一次性绑定码（wbc_ 前缀，兑换后写入换来的
// access token），或已有的 Worker Access Token（兼容旧流程）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const workerConfigEnv = "NBCO_WORKER_CONFIG"

// Config 本机配置（bind 写入，run 读取）。
type Config struct {
	Server     string `json:"server"`      // nbco 基地址，如 http://127.0.0.1:8900
	Token      string `json:"token"`       // Worker Access Token（bind 时用一次性绑定码兑换写入）
	WorkerID   int64  `json:"worker_id"`   // token 对应的 worker 用户 ID（bind 时校验写入）
	WorkerName string `json:"worker_name"` // token 对应的 worker 名字（bind 时校验写入）
	Engine     string `json:"engine"`      // 引擎名：claude | codex | builtin（内置智能体，无 CLI 也能干活），或自定义（配 bin+args）
	Bin        string `json:"bin"`         // CLI 可执行文件，默认同 engine
	// 深执行引擎可插拔（前瞻「买管道、留业务」）：把任意交互式 harness（如
	// swarm 编排器 ruflo/claude-flow 的交互 REPL）配成一个引擎，无需改代码。
	// 仍守 PTY 交互铁律——只是换掉「启动哪个 CLI、怎么判完成」。
	Args        []string `json:"args"`         // 自定义启动参数（非空则覆盖内置 claude/codex 默认参数）
	BusyPattern string   `json:"busy_pattern"` // 自定义「工作中」状态行正则（完成检测用；空=默认 "esc to interrupt"）
	// SessionRuntimeFiles / SessionRuntimeEnv extend the automatic engine
	// fingerprint for custom wrappers and provider-specific configuration. Only
	// a SHA-256 digest is sent to nbco; file contents and environment values stay
	// on the worker machine.
	SessionRuntimeFiles []string `json:"session_runtime_files,omitempty"`
	SessionRuntimeEnv   []string `json:"session_runtime_env,omitempty"`
	// SessionWorkspaces pins topic scopes to real directories. Example:
	// {"repo:nbco":"/root/src/nbco"} lets code/deploy tasks resume the nbco
	// codebase workspace while unrelated document-analysis tasks use their own
	// nbco-work session directory.
	SessionWorkspaces map[string]string `json:"session_workspaces,omitempty"`
}

func configPath(override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv(workerConfigEnv); env != "" {
		return env
	}
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".nbco-worker.json")
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bind":
		bind(os.Args[2:])
	case "bootstrap":
		bootstrap(os.Args[2:])
	case "run":
		run(os.Args[2:])
	case "once":
		once(os.Args[2:])
	case "status":
		status(os.Args[2:])
	case "doctor":
		doctor(os.Args[2:])
	case "workspace":
		workspace(os.Args[2:])
	case "logs":
		logs(os.Args[2:])
	case "install-service":
		installService(os.Args[2:])
	case "uninstall-service":
		uninstallService(os.Args[2:])
	case "service-status":
		serviceStatus(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：\n  nbco-worker bind [-config path] <server> <绑定码|token>\n  nbco-worker bootstrap [-config path] [-engine claude|codex|builtin] [-bin /path/to/cli] [-install-service=true] <server> <绑定码|token>\n  nbco-worker run [-config path] [-engine claude|codex|builtin] [-bin /path/to/cli]\n  nbco-worker once [-config path] [-engine claude|codex|builtin] [-bin /path/to/cli]\n  nbco-worker status [-config path]\n  nbco-worker doctor [-config path]\n  nbco-worker workspace [-config path]\n  nbco-worker logs [-config path] [-name name]\n  nbco-worker install-service [-config path] [-engine claude|codex|builtin] [-bin /path/to/cli] [-name name]\n  nbco-worker uninstall-service [-config path] [-name name]\n  nbco-worker service-status [-config path] [-name name]\n\n绑定码是 create_worker 给出的一次性 wbc_ 码；也兼容直接传 Worker Access Token。\n也可用 NBCO_WORKER_CONFIG 指定配置文件。")
	os.Exit(2)
}

func bind(args []string) {
	fs := flag.NewFlagSet("bind", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		usage()
	}
	cfg, path := bindConfig(*cfgFile, fs.Arg(0), fs.Arg(1), Config{Engine: "claude"})
	fmt.Printf("已绑定 worker #%d %s 到 %s，配置文件：%s。运行 nbco-worker run -config %s 上线接活。\n",
		cfg.WorkerID, cfg.WorkerName, cfg.Server, path, path)
}

func bootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	engine := fs.String("engine", "claude", "引擎：claude | codex | builtin（内置智能体）")
	bin := fs.String("bin", "", "CLI 可执行文件路径")
	install := fs.Bool("install-service", true, "绑定后安装并启动系统服务")
	name := fs.String("name", "", "服务名（同机多 worker 时建议指定）")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		usage()
	}
	cfg, path := bindConfig(*cfgFile, fs.Arg(0), fs.Arg(1), Config{Engine: *engine, Bin: *bin})
	fmt.Printf("已绑定 worker #%d %s 到 %s，配置文件：%s。\n", cfg.WorkerID, cfg.WorkerName, cfg.Server, path)
	if *install {
		if err := installServiceForConfig(path, *engine, *bin, *name); err != nil {
			log.Fatalf("安装系统服务失败: %v", err)
		}
		fmt.Println("系统服务已安装并启动。")
	}
}

func bindConfig(cfgFile, server, token string, base Config) (Config, string) {
	cfg := base
	cfg.Server = server
	cfg.Token = token
	if cfg.Engine == "" {
		cfg.Engine = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	path := configPath(cfgFile)
	redeemed := false
	if isBindCode(token) {
		// 一次性绑定码：先兑换出真正的 access token。兑换即销码+吊销旧 token，
		// 所以拿到 token 后必须【立刻落盘】——若先跑校验、校验因网络抖动失败
		// 就退出，token 只在内存里，这台机器（连同原 worker）就被彻底废掉了。
		res, err := newClient(cfg.Server, "").RedeemBindCode(ctx, token)
		if err != nil {
			cancel()
			log.Fatalf("绑定码兑换失败: %v", err)
		}
		cfg.Token = res.Token
		cfg.WorkerID = res.WorkerID
		cfg.WorkerName = res.WorkerName
		if err := saveConfig(path, cfg); err != nil {
			cancel()
			log.Fatalf("绑定码已兑换，但写配置失败: %v\n为避免泄露，Worker Access Token 不会打印到终端；请在 nbco 里给该 worker 补发一次性绑定码后重新绑定。", err)
		}
		redeemed = true
	}
	ident, err := newClient(cfg.Server, cfg.Token).Me(ctx)
	cancel()
	if err != nil {
		if redeemed {
			// token 已安全落盘，校验失败只是网络问题：提示直接上线即可，绝不 Fatal 丢绑定。
			log.Printf("警告：token 已保存到 %s，但身份校验暂时失败（%v）；网络恢复后直接运行 nbco-worker run 即可上线", path, err)
			return cfg, path
		}
		log.Fatalf("校验 Worker Access Token 失败: %v", err)
	}
	if !ident.IsWorker {
		log.Fatalf("这个凭据属于真人员工 #%d %s，不是 worker；请使用 create_worker/issue_worker_bind_code 生成的一次性 worker 绑定码", ident.ID, ident.Name)
	}
	cfg.WorkerID = ident.ID
	cfg.WorkerName = ident.Name
	if err := saveConfig(path, cfg); err != nil {
		log.Fatalf("写配置失败: %v", err)
	}
	return cfg, path
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	engine := fs.String("engine", "", "覆盖引擎：claude | codex | builtin（内置智能体）")
	bin := fs.String("bin", "", "覆盖 CLI 可执行文件路径")
	_ = fs.Parse(args)

	cfg, path := loadConfigForRun(*cfgFile, *engine, *bin)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ident := waitForWorkerIdentity(ctx, cfg)
	cfg.WorkerID = ident.ID
	cfg.WorkerName = ident.Name

	w := newWorker(cfg)
	log.Printf("nbco-worker 上线：worker=#%d %s config=%s server=%s engine=%s", cfg.WorkerID, cfg.WorkerName, path, cfg.Server, cfg.Engine)
	w.Loop(ctx)
	log.Println("已下线")
}

func once(args []string) {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	engine := fs.String("engine", "", "覆盖引擎：claude | codex | builtin（内置智能体）")
	bin := fs.String("bin", "", "覆盖 CLI 可执行文件路径")
	_ = fs.Parse(args)
	cfg, _ := loadConfigForRun(*cfgFile, *engine, *bin)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	ident := waitForWorkerIdentity(ctx, cfg)
	cfg.WorkerID = ident.ID
	cfg.WorkerName = ident.Name
	ok, err := newWorker(cfg).RunOnce(ctx)
	if err != nil {
		stop()
		log.Fatalf("单次执行失败: %v", err)
	}
	if !ok {
		log.Println("当前没有可领取任务。")
	}
	stop()
}

func loadConfigForRun(cfgFile, engine, bin string) (Config, string) {
	path := configPath(cfgFile)
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("未绑定（%v）。先运行 nbco-worker bind -config %s <server> <一次性worker绑定码>", err, path)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("配置损坏: %v", err)
	}
	if engine != "" {
		cfg.Engine = engine
	}
	if cfg.Engine == "" {
		cfg.Engine = "claude"
	}
	if bin != "" {
		cfg.Bin = bin
	}
	if cfg.Bin == "" {
		cfg.Bin = cfg.Engine
	}
	if cfg.Engine != engineBuiltin {
		if _, err := exec.LookPath(cfg.Bin); err != nil {
			log.Printf("工作机上未找到 %q，回退内置智能体模式（由中枢模型驱动）；安装对应 CLI 后重启即可恢复", cfg.Bin)
			cfg.Engine = engineBuiltin
		}
	}
	return cfg, path
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	_ = fs.Parse(args)
	cfg, path := loadConfigForRun(*cfgFile, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ident, err := newClient(cfg.Server, cfg.Token).Me(ctx)
	cancel()
	if err != nil {
		log.Fatalf("服务端身份校验失败: %v", err)
	}
	report := collectCapabilities(cfg)
	fmt.Printf("config=%s\nserver=%s\nworker=#%d %s\nengine=%s\nbin=%s\ncaps=%v\n", path, cfg.Server, ident.ID, ident.Name, report.Engine, report.CLIName, report.Capabilities)
}

func doctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	_ = fs.Parse(args)
	cfg, path := loadConfigForRun(*cfgFile, "", "")
	fmt.Printf("config: %s\n", path)
	status([]string{"-config", path})
	if cfg.Engine != engineBuiltin {
		if _, err := exec.LookPath(cfg.Bin); err != nil {
			fmt.Printf("cli: %s not found（会回退 builtin）\n", cfg.Bin)
		} else {
			fmt.Printf("cli: %s ok\n", cfg.Bin)
		}
	}
	fmt.Println("doctor: ok")
}

func workspace(args []string) {
	fs := flag.NewFlagSet("workspace", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	_ = fs.Parse(args)
	cfg, path := loadConfigForRun(*cfgFile, "", "")
	home, _ := os.UserHomeDir()
	fmt.Printf("config=%s\nbase_workspace=%s\n", path, filepath.Join(home, "nbco-work"))
	for scope, dir := range cfg.SessionWorkspaces {
		fmt.Printf("%s -> %s\n", scope, dir)
	}
}

func logs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	name := fs.String("name", "", "服务名")
	_ = fs.Parse(args)
	path := configPath(*cfgFile)
	service := serviceName(path, *name)
	out, err := platformServiceStatus(service)
	if err != nil {
		log.Fatalf("读取服务状态失败: %v\n%s", err, out)
	}
	fmt.Print(out)
}

func waitForWorkerIdentity(ctx context.Context, cfg Config) *Identity {
	client := newClient(cfg.Server, cfg.Token)
	for {
		ctxMe, cancelMe := context.WithTimeout(ctx, 30*time.Second)
		ident, err := client.Me(ctxMe)
		cancelMe()
		if err == nil {
			if !ident.IsWorker {
				log.Fatalf("这个 access token 属于真人员工 #%d %s，不是 worker；请重新 bind Worker Access Token", ident.ID, ident.Name)
			}
			return ident
		}
		log.Printf("校验 Worker Access Token 失败，10 秒后重试: %v", err)
		select {
		case <-ctx.Done():
			log.Println("启动校验已取消")
			os.Exit(0)
		case <-time.After(10 * time.Second):
		}
	}
}

func saveConfig(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".nbco-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
