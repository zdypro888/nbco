// nbco-worker：装在工作机上的 AI 员工 client。
// 轮询 nbco 领取分配给自己的任务，用 PTY 驱动 claude/codex 交互式执行，
// 把终端输出实时回传作为进度，完成后提交验收。
//
//	nbco-worker bind [-config path] <server> <token>   # 绑定，写本机配置
//	nbco-worker bootstrap [-config path] [-engine claude|codex] [-bin /path/to/cli] <server> <token>
//	                                                       # 绑定并安装为系统服务
//	nbco-worker run [-config path]                      # 上线接活
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const workerConfigEnv = "NBCO_WORKER_CONFIG"

// Config 本机配置（bind 写入，run 读取）。
type Config struct {
	Server     string `json:"server"`      // nbco 基地址，如 http://127.0.0.1:8900
	Token      string `json:"token"`       // Worker Access Token（create_worker 返回）
	WorkerID   int64  `json:"worker_id"`   // token 对应的 worker 用户 ID（bind 时校验写入）
	WorkerName string `json:"worker_name"` // token 对应的 worker 名字（bind 时校验写入）
	Engine     string `json:"engine"`      // 引擎名：内置 claude | codex，或自定义（配 bin+args）
	Bin        string `json:"bin"`         // CLI 可执行文件，默认同 engine
	// 深执行引擎可插拔（前瞻「买管道、留业务」）：把任意交互式 harness（如
	// swarm 编排器 ruflo/claude-flow 的交互 REPL）配成一个引擎，无需改代码。
	// 仍守 PTY 交互铁律——只是换掉「启动哪个 CLI、怎么判完成」。
	Args        []string `json:"args"`         // 自定义启动参数（非空则覆盖内置 claude/codex 默认参数）
	BusyPattern string   `json:"busy_pattern"` // 自定义「工作中」状态行正则（完成检测用；空=默认 "esc to interrupt"）
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
	fmt.Fprintln(os.Stderr, "用法：\n  nbco-worker bind [-config path] <server> <token>\n  nbco-worker bootstrap [-config path] [-engine claude|codex] [-bin /path/to/cli] [-install-service=true] <server> <token>\n  nbco-worker run [-config path] [-engine claude|codex] [-bin /path/to/cli]\n  nbco-worker install-service [-config path] [-engine claude|codex] [-bin /path/to/cli] [-name name]\n  nbco-worker uninstall-service [-config path] [-name name]\n  nbco-worker service-status [-config path] [-name name]\n\n也可用 NBCO_WORKER_CONFIG 指定配置文件。")
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
	engine := fs.String("engine", "claude", "引擎：claude | codex")
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
	defer cancel()
	ident, err := newClient(cfg.Server, cfg.Token).Me(ctx)
	if err != nil {
		log.Fatalf("校验 Worker Access Token 失败: %v", err)
	}
	if !ident.IsWorker {
		log.Fatalf("这个 access token 属于真人员工 #%d %s，不是 worker；请使用 create_worker 返回的 Worker Access Token", ident.ID, ident.Name)
	}
	cfg.WorkerID = ident.ID
	cfg.WorkerName = ident.Name
	path := configPath(cfgFile)
	if err := saveConfig(path, cfg); err != nil {
		log.Fatalf("写配置失败: %v", err)
	}
	return cfg, path
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgFile := fs.String("config", "", "配置文件路径（也可用 NBCO_WORKER_CONFIG）")
	engine := fs.String("engine", "", "覆盖引擎：claude | codex")
	bin := fs.String("bin", "", "覆盖 CLI 可执行文件路径")
	_ = fs.Parse(args)

	path := configPath(*cfgFile)
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("未绑定（%v）。先运行 nbco-worker bind -config %s <server> <Worker接入Token>", err, path)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("配置损坏: %v", err)
	}
	if *engine != "" {
		cfg.Engine = *engine
	}
	if cfg.Engine == "" {
		cfg.Engine = "claude"
	}
	if *bin != "" {
		cfg.Bin = *bin
	}
	if cfg.Bin == "" {
		cfg.Bin = cfg.Engine
	}

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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
