// nbco-worker：装在工作机上的 AI 员工 client。
// 轮询 nbco 领取分配给自己的任务，用 PTY 驱动 claude/codex 交互式执行，
// 把终端输出实时回传作为进度，完成后提交验收。
//
//	nbco-worker bind <server> <token>   # 绑定，写本机配置
//	nbco-worker run                      # 上线接活
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
)

// Config 本机配置（bind 写入，run 读取）。
type Config struct {
	Server string `json:"server"` // nbco 基地址，如 http://127.0.0.1:8900
	Token  string `json:"token"`  // worker 接入令牌
	Engine string `json:"engine"` // 引擎名：内置 claude | codex，或自定义（配 bin+args）
	Bin    string `json:"bin"`    // CLI 可执行文件，默认同 engine
	// 深执行引擎可插拔（前瞻「买管道、留业务」）：把任意交互式 harness（如
	// swarm 编排器 ruflo/claude-flow 的交互 REPL）配成一个引擎，无需改代码。
	// 仍守 PTY 交互铁律——只是换掉「启动哪个 CLI、怎么判完成」。
	Args        []string `json:"args"`         // 自定义启动参数（非空则覆盖内置 claude/codex 默认参数）
	BusyPattern string   `json:"busy_pattern"` // 自定义「工作中」状态行正则（完成检测用；空=默认 "esc to interrupt"）
}

func configPath() string {
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
	case "run":
		run(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：\n  nbco-worker bind <server> <token>\n  nbco-worker run [-engine claude|codex] [-bin /path/to/cli]")
	os.Exit(2)
}

func bind(args []string) {
	if len(args) < 2 {
		usage()
	}
	cfg := Config{Server: args[0], Token: args[1], Engine: "claude"}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath(), data, 0o600); err != nil {
		log.Fatalf("写配置失败: %v", err)
	}
	fmt.Printf("已绑定到 %s。运行 nbco-worker run 上线接活。\n", cfg.Server)
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	engine := fs.String("engine", "", "覆盖引擎：claude | codex")
	bin := fs.String("bin", "", "覆盖 CLI 可执行文件路径")
	_ = fs.Parse(args)

	data, err := os.ReadFile(configPath())
	if err != nil {
		log.Fatalf("未绑定（%v）。先运行 nbco-worker bind <server> <token>", err)
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

	w := newWorker(cfg)
	log.Printf("nbco-worker 上线：server=%s engine=%s", cfg.Server, cfg.Engine)
	w.Loop(ctx)
	log.Println("已下线")
}
