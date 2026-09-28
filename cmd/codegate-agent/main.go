// Command codegate-agent 是 CodeGate 的本机 Agent。
//
// 它常驻在你要远程访问的机器上，只出站连接 Server（不需要公网 IP、
// 不需要端口映射），把本机 PTY 的字节透明地搬给浏览器。
//
// 用法：
//
//	codegate-agent run       启动 Agent（常驻）
//	codegate-agent pair      生成配对码，把本机绑定到账号
//	codegate-agent status    查看本机身份与状态目录
//	codegate-agent config    打印生效的配置
//	codegate-agent doctor    环境自检（PTY / 目录 / 密钥 / 配置）
//	codegate-agent version   版本
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/jojo/codegate/internal/agent"
	"github.com/jojo/codegate/internal/terminal"
)

// version 由构建时注入：-ldflags "-X main.version=..."
var version = "dev"

func main() {
	// 把构建期版本同步给 agent 包，让 agent.hello 里报的版本是真的。
	if version != "" {
		agent.Version = version
	}

	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "pair":
		return cmdPair(rest)
	case "status":
		return cmdStatus(rest)
	case "config":
		return cmdConfig(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "version", "-v", "--version":
		fmt.Printf("codegate-agent %s\n", agent.Version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("未知子命令 %q", cmd)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `CodeGate Agent —— 在本机运行，让浏览器安全访问原生 CLI

用法:
  codegate-agent <子命令> [参数]

子命令:
  run       启动 Agent（常驻，断线自动重连）
  pair      生成配对码，把本机绑定到账号
  status    查看本机设备身份与状态目录
  config    打印生效的配置（含默认值）
  doctor    环境自检：PTY / 目录 / 密钥 / 配置
  version   打印版本

常用参数（run / pair / doctor 支持）:
  -config <路径>     配置文件路径（默认见下）
  -server <地址>     覆盖 server_url，如 wss://example.com/ws/agent
  -state-dir <路径>  覆盖状态目录（存放 device.key）

环境变量（优先级高于配置文件）:
  CODEGATE_SERVER_URL    同 -server
  CODEGATE_DEVICE_NAME   设备显示名
  CODEGATE_STATE_DIR     同 -state-dir
  CODEGATE_INSECURE=1    允许 ws:// 与自签证书（仅限开发）

配置文件默认路径:
  `+defaultConfigPath()+`
`)
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	serverURL := fs.String("server", "", "覆盖 server_url")
	stateDir := fs.String("state-dir", "", "覆盖状态目录")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath, *serverURL, *stateDir)
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}

	// Ctrl+C / SIGTERM 触发优雅退出：关闭全部会话再退出，
	// 让子进程收到正确的终止信号，也不留下孤儿 conhost。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("CodeGate Agent 启动",
		"version", agent.Version,
		"device_id", a.Identity().DeviceID,
		"server", cfg.ServerURL,
		"roots", len(cfg.AllowedRoots),
		"commands", len(cfg.AllowedCommands))

	return a.Run(ctx)
}

// ---------------------------------------------------------------------------
// pair
// ---------------------------------------------------------------------------

func cmdPair(args []string) error {
	fs := newFlagSet("pair")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	serverURL := fs.String("server", "", "覆盖 server_url")
	stateDir := fs.String("state-dir", "", "覆盖状态目录")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath, *serverURL, *stateDir)
	if err != nil {
		return err
	}

	a, err := agent.New(cfg, newLogger(cfg))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return a.Pair(ctx, os.Stdout)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	stateDir := fs.String("state-dir", "", "覆盖状态目录")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath, "", *stateDir)
	if err != nil {
		// status 的价值就在于"配置坏了也能看到问题"，所以这里不直接退出。
		fmt.Fprintf(os.Stderr, "警告: 配置有问题（%v），下面显示的是默认值\n\n", err)
		cfg = agent.DefaultConfig()
		if *stateDir != "" {
			cfg.StateDir = *stateDir
		}
		cfg, _ = cfg.Prepare()
	}

	fmt.Printf("配置文件    %s\n", *configPath)
	fmt.Printf("状态目录    %s\n", cfg.StateDir)
	fmt.Printf("设备名称    %s\n", cfg.DeviceName)
	fmt.Printf("Server      %s\n", orDash(cfg.ServerURL))

	// 设备身份：能读出来就显示，读不出来说明还没配对过。
	id, err := agent.LoadOrCreate(cfg.StateDir)
	if err != nil {
		fmt.Printf("设备 ID     (不可用: %v)\n", err)
	} else {
		fmt.Printf("设备 ID     %s\n", id.DeviceID)
		fmt.Printf("公钥        %s\n", id.PublicKeyB64())
	}

	fmt.Printf("工作区      %s\n", orDash(strings.Join(cfg.AllowedRoots, ", ")))
	fmt.Printf("预置命令    %d 条\n", len(cfg.AllowedCommands))
	return nil
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func cmdConfig(args []string) error {
	fs := newFlagSet("config")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath, "", "")
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// checkResult 是一项自检的结果。
type checkResult struct {
	name string
	err  error
	note string
}

func cmdDoctor(args []string) error {
	fs := newFlagSet("doctor")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	serverURL := fs.String("server", "", "覆盖 server_url")
	stateDir := fs.String("state-dir", "", "覆盖状态目录")
	_ = fs.Parse(args)

	fmt.Println("CodeGate Agent 环境自检")
	fmt.Println(strings.Repeat("-", 56))

	var results []checkResult

	// 1. 配置。
	cfg, err := loadConfig(*configPath, *serverURL, *stateDir)
	results = append(results, checkResult{
		name: "配置文件",
		err:  err,
		note: *configPath,
	})
	if err != nil {
		// 配置不对后面几项没法查，直接出结论。
		report(results)
		return errors.New("配置未通过校验")
	}

	// 2. PTY 可用性 —— 这是 Agent 能不能干活的前提。
	ptyErr := terminal.Available()
	ptyNote := "ConPTY 可用"
	if ptyErr != nil {
		ptyNote = ptyErr.Error()
	}
	results = append(results, checkResult{name: "终端 (PTY)", err: ptyErr, note: ptyNote})

	// 3. 状态目录可写。
	dirErr := checkWritableDir(cfg.StateDir)
	results = append(results, checkResult{
		name: "状态目录可写",
		err:  dirErr,
		note: cfg.StateDir,
	})

	// 4. 设备密钥。
	var keyNote string
	id, keyErr := agent.LoadOrCreate(cfg.StateDir)
	if keyErr == nil {
		keyNote = "device_id = " + id.DeviceID
	}
	results = append(results, checkResult{name: "设备密钥", err: keyErr, note: keyNote})

	// 5. 工作区目录存在性。
	// 不存在不算致命（用户可能还没建），但要提醒 —— 否则第一次
	// 创建会话时会以 cwd_not_allowed 失败，而原因很难猜。
	for _, root := range cfg.AllowedRoots {
		var rootErr error
		if fi, err := os.Stat(root); err != nil {
			rootErr = fmt.Errorf("不存在: %w", err)
		} else if !fi.IsDir() {
			rootErr = errors.New("不是目录")
		}
		results = append(results, checkResult{name: "工作区", err: rootErr, note: root})
	}

	// 6. 命令白名单的可执行文件是否存在。
	for _, cmd := range cfg.AllowedCommands {
		path, err := exec.LookPath(cmd.Command)
		note := cmd.ID + " -> " + cmd.Command + "（找不到可执行文件）"
		if err == nil {
			note = cmd.ID + " -> " + path
		}
		results = append(results, checkResult{
			name: "命令",
			// 找不到不算致命：可能是 PATH 问题，也可能用户根本不用这条。
			// 报出来让用户自己判断。
			err:  nil,
			note: note,
		})
	}

	// 7. 平台提示。
	results = append(results, checkResult{
		name: "平台",
		note: fmt.Sprintf("%s/%s, Agent %s", runtime.GOOS, runtime.GOARCH, agent.Version),
	})

	// 8. Windows 上的一条重要提醒。
	if runtime.GOOS == "windows" {
		results = append(results, checkResult{
			name: "Ctrl+C 限制",
			note: "shell 会话（cmd/PowerShell）无法接收中断信号，" +
				"TUI（Claude Code 等）正常。详见架构文档 §6.5",
		})
	}

	report(results)

	// 只有"致命项"失败才返回非零。
	for _, r := range results {
		if r.err != nil && r.name != "命令" {
			return errors.New("自检发现问题")
		}
	}
	fmt.Println("\n自检通过。")
	return nil
}

func report(results []checkResult) {
	for _, r := range results {
		switch {
		case r.err != nil:
			fmt.Printf("  [!!] %-16s %s\n", r.name, r.err)
			if r.note != "" && r.note != r.err.Error() {
				fmt.Printf("       %s\n", r.note)
			}
		case r.note != "":
			fmt.Printf("  [ok] %-16s %s\n", r.name, r.note)
		default:
			fmt.Printf("  [ok] %s\n", r.name)
		}
	}
}

func checkWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	// 自定义 Usage，避免默认输出把子命令参数和顶层用法混在一起。
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: codegate-agent %s [参数]\n\n参数:\n", name)
		fs.PrintDefaults()
	}
	return fs
}

func defaultConfigPath() string {
	return filepath.Join(agent.DefaultStateDir(), "agent.json")
}

// loadConfig 按「文件 → 环境变量 → 命令行」的顺序加载并合并配置。
//
// 命令行优先级最高：它是用户在这一刻的显式意图，应当能压过
// 任何持久化下来的设置。
func loadConfig(path, serverURL, stateDir string) (agent.Config, error) {
	cfg, err := agent.Load(path)
	if err != nil {
		return agent.Config{}, err
	}

	cfg.ApplyEnv(os.Getenv)

	if serverURL != "" {
		cfg.ServerURL = serverURL
	}
	if stateDir != "" {
		cfg.StateDir = stateDir
	}

	return cfg.Prepare()
}

func newLogger(cfg agent.Config) *slog.Logger {
	// 用 TextHandler 而不是 JSON：Agent 主要跑在用户的机器上，
	// 输出要能被人直接看懂。结构化日志留给服务端（那边才需要采集）。
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func orDash(s string) string {
	if s == "" {
		return "（未设置）"
	}
	return s
}
