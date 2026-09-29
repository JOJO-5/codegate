// Command codegate-server 是 CodeGate 的服务端。
//
// 它跑在一台有公网地址的机器上（或挂在反代后面），负责三件事：
//
//  1. 给浏览器提供 REST API（登录、设备列表、配对、审计）
//  2. 维护两条 WebSocket 通路：Agent 侧（Ed25519 认证）与浏览器侧（一次性票据）
//  3. 把两侧的终端字节互相转发，并做授权、限流、审计
//
// 它**不**保存终端内容 —— 屏幕状态在 Agent 的 ring buffer 里，
// 服务端只做转发。所以服务端被拖库不会泄露用户敲过的命令输出。
//
// 用法：
//
//	codegate-server serve            启动服务（常驻）
//	codegate-server user add         创建账号
//	codegate-server user list        列出账号
//	codegate-server user disable     停用账号（立刻生效）
//	codegate-server user enable      重新启用账号
//	codegate-server user passwd      重置密码（忘记密码时唯一的出路）
//	codegate-server config           打印生效的配置
//	codegate-server doctor           环境自检（目录 / 数据库 / 密钥 / 端口）
//	codegate-server version          版本
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/config"
	"github.com/jojo/codegate/internal/logging"
	"github.com/jojo/codegate/internal/server"
	"github.com/jojo/codegate/internal/storage"
)

// version 由构建时注入：-ldflags "-X main.version=..."
var version = "dev"

func main() {
	// 把构建期版本同步给 server 包，让 /api/v1/version 报的是真版本。
	if version != "" {
		server.Version = version
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
	case "serve":
		return cmdServe(rest)
	case "user":
		return cmdUser(rest)
	case "config":
		return cmdConfig(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "version", "-v", "--version":
		fmt.Printf("codegate-server %s\n", server.Version)
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
	fmt.Fprint(w, `CodeGate Server —— 让浏览器安全访问你家里的机器

用法:
  codegate-server <子命令> [参数]

子命令:
  serve                启动服务（常驻，Ctrl+C 优雅退出）
  user add             创建账号（首次部署时用）
  user list            列出所有账号
  user disable <邮箱>  停用账号（对已签发的 token 立刻生效）
  user enable  <邮箱>  重新启用账号
  user passwd  <邮箱>  重置密码
  config               打印生效的配置（含默认值）
  doctor               环境自检：目录 / 数据库 / JWT 密钥 / 监听端口
  version              打印版本

通用参数:
  -config <路径>   配置文件路径（JSON，可省略）
  -db <路径>       覆盖数据库路径

环境变量（优先级高于配置文件）:
  CODEGATE_LISTEN            监听地址，如 :8080 或 127.0.0.1:8080
  CODEGATE_BASE_URL          对外地址，如 https://codegate.example.com
  CODEGATE_DB_PATH           SQLite 文件路径
  CODEGATE_JWT_SECRET        JWT 签名密钥（至少 32 字节）
  CODEGATE_JWT_SECRET_FILE   从文件读密钥（推荐 —— 不出现在进程环境里）
  CODEGATE_ALLOW_SIGNUP      是否开放注册（true/false，默认 false）
  CODEGATE_ALLOWED_ORIGINS   允许的浏览器来源，逗号分隔
  CODEGATE_TRUSTED_PROXIES   可信反代地址，逗号分隔（决定是否信 X-Forwarded-For）
  CODEGATE_LOG_LEVEL         debug | info | warn | error

默认数据库:
  `+config.DefaultDBPath()+`
`)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := newFlagSet("serve")
	configPath := fs.String("config", "", "配置文件路径")
	dbPath := fs.String("db", "", "覆盖数据库路径")
	logLevel := fs.String("log-level", "", "覆盖日志级别")
	textLog := fs.Bool("text-log", false, "用人类可读的文本日志（默认 JSON）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := logging.Setup(logging.Options{Level: cfg.LogLevel, Text: *textLog})
	for _, warn := range cfg.Warnings() {
		log.Warn(warn)
	}

	// 数据库目录可能还不存在（首次部署）。EnsureDir 用 0700 ——
	// 这里存的是用户表、刷新令牌哈希、设备公钥，都不该被别的用户读到。
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := config.EnsureDir(dir); err != nil {
			return fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
		}
	}

	ctx := context.Background()
	store, err := storage.OpenSQLite(ctx, storage.SQLiteOptions{
		Path:   cfg.DBPath,
		Logger: log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("数据库迁移失败: %w", err)
	}

	srv, err := server.New(server.Options{Config: cfg, Logger: log, Store: store})
	if err != nil {
		return err
	}

	// Ctrl+C / SIGTERM → 优雅退出。
	//
	// ★ 这一步不能省。http.Server.Shutdown **不会**关闭被 hijack 的连接
	// （WebSocket 就是），不显式处理的话进程会一直卡着等它们自然断开 ——
	// 而客户端在等我们发消息，双方互等。server.Shutdown 里已经处理了这点。
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-runCtx.Done()
		log.Info("收到退出信号，正在优雅关闭……")
	}()

	log.Info("CodeGate Server 启动",
		"version", server.Version,
		"listen", cfg.Listen,
		"db", cfg.DBPath,
		"tls", cfg.TLS.Enabled,
		"allow_signup", cfg.AllowSignup,
		"origins", len(cfg.AllowedOrigins),
	)

	if err := srv.Serve(runCtx); err != nil {
		return err
	}
	log.Info("已退出")
	return nil
}

// ---------------------------------------------------------------------------
// user
// ---------------------------------------------------------------------------

func cmdUser(args []string) error {
	if len(args) == 0 {
		return errors.New("user 需要子命令：add | list | disable | enable | passwd")
	}

	sub, rest := args[0], args[1:]
	fs := newFlagSet("user " + sub)
	configPath := fs.String("config", "", "配置文件路径")
	dbPath := fs.String("db", "", "覆盖数据库路径")
	email := fs.String("email", "", "邮箱（等价于位置参数）")
	password := fs.String("password", "", "密码（省略则交互式输入；仅 add / passwd 用）")
	role := fs.String("role", "user", "角色（仅 add 用）")

	positional, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}

	// 管理命令不该因为 JWT secret 没配就拒绝执行 ——
	// 「密钥丢了，先建个账号进去看看」是很常见的排障路径。
	// Validate 会检查 secret，所以这里只校验路径。
	if strings.TrimSpace(cfg.DBPath) == "" {
		return errors.New("数据库路径为空，请用 -db 指定")
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return fmt.Errorf("数据库 %s 不存在或不可读（先用 serve 启动一次完成初始化）: %w",
			cfg.DBPath, err)
	}

	ctx := context.Background()
	store, err := storage.OpenSQLite(ctx, storage.SQLiteOptions{
		Path:   cfg.DBPath,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// 位置参数补位：`user disable foo@bar.com` 比 `user disable -email foo@bar.com` 顺手。
	if *email == "" && len(positional) > 0 {
		*email = positional[0]
	}

	switch sub {
	case "add":
		return userAdd(ctx, store, *email, *password, *role)
	case "list":
		return userList(ctx, store)
	case "disable":
		return userSetDisabled(ctx, store, *email, true)
	case "enable":
		return userSetDisabled(ctx, store, *email, false)
	case "passwd":
		return userPasswd(ctx, store, *email, *password)
	default:
		return fmt.Errorf("未知的 user 子命令 %q（可选 add | list | disable | enable | passwd）", sub)
	}
}

func userAdd(ctx context.Context, store storage.Store, email, password, role string) error {
	email = normalizeEmail(email)
	if email == "" {
		return errors.New("缺少邮箱：codegate-server user add -email you@example.com")
	}
	if !validEmail(email) {
		return fmt.Errorf("邮箱格式不正确: %q", email)
	}

	// 先查重再问密码：让用户输完密码才发现邮箱重复是很糟的体验。
	if _, err := store.UserByEmail(ctx, email); err == nil {
		return fmt.Errorf("邮箱 %s 已存在", email)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}

	pw, err := resolvePassword(password, true)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(pw); err != nil {
		return err
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	u := &storage.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := store.CreateUser(ctx, u); err != nil {
		return err
	}

	fmt.Printf("已创建账号\n  邮箱  %s\n  角色  %s\n  ID    %s\n", u.Email, u.Role, u.ID)
	fmt.Println("\n现在可以用它登录浏览器端，或直接调 POST /api/v1/auth/login。")
	return nil
}

func userList(ctx context.Context, store storage.Store) error {
	users, err := store.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("（还没有任何账号 —— 用 `user add` 或直接 POST /api/v1/auth/register 建第一个）")
		return nil
	}

	fmt.Printf("%-40s  %-8s  %-6s  %s\n", "邮箱", "状态", "角色", "创建时间")
	for _, u := range users {
		status := "正常"
		if u.Disabled {
			// 用带标记的字面量而不是颜色码：管理命令的输出常被
			// 重定向到文件或贴进 issue，ANSI 转义在那里是噪声。
			status = "!! 停用"
		}
		fmt.Printf("%-40s  %-8s  %-6s  %s\n",
			u.Email, status, u.Role, u.CreatedAt.Format(time.RFC3339))
	}
	fmt.Printf("\n共 %d 个账号\n", len(users))
	return nil
}

func userSetDisabled(ctx context.Context, store storage.Store, email string, disabled bool) error {
	email = normalizeEmail(email)
	if email == "" {
		return errors.New("缺少邮箱：codegate-server user disable you@example.com")
	}

	u, err := store.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("账号 %s 不存在", email)
		}
		return err
	}

	if err := store.SetUserDisabled(ctx, u.ID, disabled, time.Now().UTC()); err != nil {
		return err
	}

	if disabled {
		fmt.Printf("已停用 %s\n", u.Email)
		// ★ 必须告诉用户这一点，否则他会以为「停用了但对方还能用」
		// 是漏洞。事实是：requireAuth 每次请求都查库，
		// 所以停用在**下一个请求**就生效，不需要等 token 过期。
		fmt.Println("该账号的 access token 会在下一个请求就被拒（无需等待过期）。")
	} else {
		fmt.Printf("已启用 %s\n", u.Email)
	}
	return nil
}

func userPasswd(ctx context.Context, store storage.Store, email, password string) error {
	email = normalizeEmail(email)
	if email == "" {
		return errors.New("缺少邮箱：codegate-server user passwd you@example.com")
	}

	u, err := store.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("账号 %s 不存在", email)
		}
		return err
	}

	pw, err := resolvePassword(password, true)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(pw); err != nil {
		return err
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if err := store.UpdateUserPassword(ctx, u.ID, hash, now); err != nil {
		return err
	}

	// ★ 重置密码必须同时吊销所有刷新令牌。
	//
	// 管理员重置密码的典型场景是「用户说账号被盗了」。不吊销的话，
	// 攻击者手里的 refresh token 还能继续换 access token ——
	// 重置密码这个动作就完全没达到它该有的效果。
	n, err := store.RevokeUserRefreshTokens(ctx, u.ID, now)
	if err != nil {
		return err
	}

	fmt.Printf("已重置 %s 的密码\n", u.Email)
	fmt.Printf("同时吊销了 %d 个刷新令牌（所有已登录设备需重新登录）\n", n)
	return nil
}

// ---------------------------------------------------------------------------
// config / doctor
// ---------------------------------------------------------------------------

func cmdConfig(args []string) error {
	fs := newFlagSet("config")
	configPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	// ★ 打出来之前先把 secret 抹掉。
	//
	// 这个命令的输出会被贴进 issue、贴进聊天、写进部署记录。
	// 一个 `"JWTSecret": "..."` 就这么泄露了 —— 而拿到它
	// 等于可以伪造任意用户的 access token。
	out := map[string]any{}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	if len(cfg.JWTSecret) > 0 {
		out["JWTSecret"] = fmt.Sprintf("<已设置，%d 字节>", len(cfg.JWTSecret))
	} else {
		out["JWTSecret"] = "<未设置>"
	}

	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))

	for _, warn := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "警告: "+warn)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "\n配置校验失败:")
		fmt.Fprintln(os.Stderr, err.Error())
	}
	return nil
}

// check 是一项自检结果。
type check struct {
	name string
	err  error
	note string
}

func cmdDoctor(args []string) error {
	fs := newFlagSet("doctor")
	configPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	var checks []check

	// ---- JWT 密钥 ----
	switch {
	case len(cfg.JWTSecret) == 0:
		checks = append(checks, check{"JWT 密钥", errors.New("未配置"), ""})
	case len(cfg.JWTSecret) < 32:
		checks = append(checks, check{"JWT 密钥",
			fmt.Errorf("只有 %d 字节，HS256 要求至少 32", len(cfg.JWTSecret)), ""})
	default:
		checks = append(checks, check{"JWT 密钥", nil,
			fmt.Sprintf("%d 字节", len(cfg.JWTSecret))})
	}

	// ---- 数据目录可写 ----
	dbDir := filepath.Dir(cfg.DBPath)
	if dbDir == "" || dbDir == "." {
		dbDir = "."
	}
	checks = append(checks, check{"数据目录可写", probeWritable(dbDir), dbDir})

	// ---- 数据库 ----
	dbCheck, dbNote := probeDatabase(cfg.DBPath)
	checks = append(checks, check{"数据库", dbCheck, dbNote})

	// ---- 监听端口 ----
	portErr := probeListen(cfg.Listen)
	checks = append(checks, check{"监听端口可用", portErr, cfg.Listen})

	// ---- Origin 白名单 ----
	if len(cfg.AllowedOrigins) == 0 {
		// 不是错误，但必须说出来：空白名单意味着**不校验** Origin，
		// 任何网页都能对你的 WS 端点发起连接。
		checks = append(checks, check{"Origin 白名单",
			errors.New("为空（不校验 Origin，仅限开发）"), ""})
	} else {
		checks = append(checks, check{"Origin 白名单", nil,
			strings.Join(cfg.AllowedOrigins, ", ")})
	}

	// ---- 反代信任 ----
	if len(cfg.TrustedProxies) == 0 {
		checks = append(checks, check{"可信反代", nil,
			"未配置 → 不信任 X-Forwarded-For（正确，除非你在反代后面）"})
	} else {
		checks = append(checks, check{"可信反代", nil,
			strings.Join(cfg.TrustedProxies, ", ")})
	}

	// ---- 输出 ----
	fmt.Printf("CodeGate Server %s 自检\n\n", server.Version)
	failed := 0
	for _, c := range checks {
		switch {
		case c.err == nil:
			fmt.Printf("  [ OK ] %-16s %s\n", c.name, c.note)
		default:
			fmt.Printf("  [!!]  %-16s %v", c.name, c.err)
			if c.note != "" {
				fmt.Printf("  (%s)", c.note)
			}
			fmt.Println()
			failed++
		}
	}

	if err := cfg.Validate(); err != nil {
		fmt.Println()
		fmt.Println("配置校验未通过：")
		fmt.Println("  " + strings.ReplaceAll(err.Error(), "\n", "\n  "))
		failed++
	}

	fmt.Println()
	if failed > 0 {
		fmt.Printf("有 %d 项需要注意。\n", failed)
		// 刻意不返回非零退出码：doctor 是**诊断**工具，
		// 它的输出本身就是结果。「Origin 白名单为空」这类提示
		// 在开发环境是完全正常的，为此让脚本失败会让人干脆不跑它。
		return nil
	}
	fmt.Println("全部通过。")
	return nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseArgs 解析「标志与位置参数可以任意交错」的命令行，返回位置参数。
//
// ★ 标准库的 flag 包在遇到**第一个非标志参数时就停止解析**。
//
// 于是 `user passwd bob@example.com -password xxx` 会静默忽略 -password：
// 它被当成位置参数收下，password 变量保持空值，程序转去弹交互式输入。
// 用户看到的是「密码: 」然后 EOF 报错，完全联想不到是自己把标志写在了后面。
//
// 管理命令是人手敲的，参数顺序不该成为陷阱。这里做一次重排：
// 先把标志（连同它需要的值）挑出来，剩下的才是位置参数。
//
// 顺带保留了 `--` 分隔符的标准语义：`--` 之后的一切都是位置参数，
// 即使它长得像标志（邮箱里不会有 `-` 开头，但路径可能有）。
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		// 位置参数：不是以 '-' 开头，或只有一个 '-'（后者常表示 stdin）。
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)

		name := strings.TrimLeft(a, "-")
		// `-flag=value` 自带值，不需要往后看。
		if strings.ContainsRune(name, '=') {
			continue
		}

		// 未知标志：原样交给 flag 包，由它报「flag provided but not defined」——
		// 那比我们在这里猜它要不要值更准确。
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		// bool 标志不带值。用 IsBoolFlag 判断而不是硬编码名字 ——
		// flag 包正是用这个可选接口来区分 `-v` 和 `-v true` 的。
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		// 需要值的标志：把下一个参数一并收进 flags。
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}

// loadConfig 读配置文件（若存在）并叠加环境变量。
//
// ★ 顺序是「默认值 → 配置文件 → 环境变量」，后者覆盖前者。
// 部署时容器编排往往只能给环境变量，而本机调试更愿意改文件；
// 两者都要支持，且环境变量优先（因为它更接近运行时、更不容易被提交进仓库）。
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	if path == "" {
		return cfg, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("配置文件 %s 不存在", path)
		}
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 用一个中性的中间结构，避免直接把 JSON 解进 Config ——
	// Config 里有 []byte 字段（JWTSecret），JSON 会把它当成 base64，
	// 于是文件里写一个明文密钥就会被解析失败或解出乱码。
	var file struct {
		Listen         *string   `json:"listen"`
		BaseURL        *string   `json:"base_url"`
		DBPath         *string   `json:"db_path"`
		AgentUpdatesDir *string `json:"agent_updates_dir"`
		JWTSecret      *string   `json:"jwt_secret"`
		AllowSignup    *bool     `json:"allow_signup"`
		AllowedOrigins *[]string `json:"allowed_origins"`
		TrustedProxies *[]string `json:"trusted_proxies"`
		LogLevel       *string   `json:"log_level"`
		AccessTTL      *string   `json:"access_token_ttl"`
		RefreshTTL     *string   `json:"refresh_token_ttl"`
		PairingTTL     *string   `json:"pairing_code_ttl"`
		SendQueueSize  *int      `json:"send_queue_size"`
		TLS            *struct {
			Enabled  bool   `json:"enabled"`
			CertFile string `json:"cert_file"`
			KeyFile  string `json:"key_file"`
		} `json:"tls"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	if file.Listen != nil {
		cfg.Listen = *file.Listen
	}
	if file.BaseURL != nil {
		cfg.BaseURL = strings.TrimRight(*file.BaseURL, "/")
	}
	if file.DBPath != nil {
		cfg.DBPath = *file.DBPath
	}
	if file.AgentUpdatesDir != nil {
		cfg.AgentUpdatesDir = *file.AgentUpdatesDir
	}
	// 配置文件里的密钥只在环境变量没给时才生效 ——
	// 环境变量优先，理由同上。
	if file.JWTSecret != nil && len(cfg.JWTSecret) == 0 {
		cfg.JWTSecret = []byte(*file.JWTSecret)
	}
	if file.AllowSignup != nil {
		cfg.AllowSignup = *file.AllowSignup
	}
	if file.AllowedOrigins != nil {
		cfg.AllowedOrigins = *file.AllowedOrigins
	}
	if file.TrustedProxies != nil {
		cfg.TrustedProxies = *file.TrustedProxies
	}
	if file.LogLevel != nil {
		cfg.LogLevel = *file.LogLevel
	}
	if file.SendQueueSize != nil {
		cfg.SendQueueSize = *file.SendQueueSize
	}
	if file.TLS != nil {
		cfg.TLS = config.TLSConfig{
			Enabled:  file.TLS.Enabled,
			CertFile: file.TLS.CertFile,
			KeyFile:  file.TLS.KeyFile,
		}
	}
	for _, it := range []struct {
		src  *string
		dst  *time.Duration
		name string
	}{
		{file.AccessTTL, &cfg.AccessTokenTTL, "access_token_ttl"},
		{file.RefreshTTL, &cfg.RefreshTokenTTL, "refresh_token_ttl"},
		{file.PairingTTL, &cfg.PairingCodeTTL, "pairing_code_ttl"},
	} {
		if it.src == nil {
			continue
		}
		d, err := time.ParseDuration(*it.src)
		if err != nil {
			return nil, fmt.Errorf("配置项 %s 不是合法时长（如 15m / 30s）: %q", it.name, *it.src)
		}
		*it.dst = d
	}

	return cfg, nil
}

// resolvePassword 取密码：参数给了就用参数，否则交互式输入。
//
// ★ 交互式输入走 stdin 而不是终端回显 —— 密码不该出现在 shell 历史里，
// 也不该被旁边的人看到。这里刻意不引 golang.org/x/term：
// 多一个依赖只为关掉回显不值得，而 `-password` 参数已经覆盖了
// 脚本化场景（CI 里本来也没有终端）。
func resolvePassword(given string, confirm bool) (string, error) {
	if given != "" {
		return given, nil
	}

	fmt.Fprint(os.Stderr, "密码: ")
	pw, err := readLine(os.Stdin)
	if err != nil {
		return "", err
	}
	pw = strings.TrimRight(pw, "\r\n")
	if pw == "" {
		return "", errors.New("密码为空")
	}

	if confirm {
		fmt.Fprint(os.Stderr, "再输一次: ")
		again, err := readLine(os.Stdin)
		if err != nil {
			return "", err
		}
		if strings.TrimRight(again, "\r\n") != pw {
			return "", errors.New("两次输入的密码不一致")
		}
	}
	return pw, nil
}

func readLine(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validEmail 做宽松校验，不追 RFC 5322。
//
// 严格的邮箱正则是有名的反模式：它们既拦不住真正非法的地址
// （只有投递才能确认），又会拒绝一批合法但少见的写法。
// 真正能确认邮箱有效的唯一方式是给它发一封信。
func validEmail(s string) bool {
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}

// probeWritable 检查目录是否可写（真的写一个文件再删掉）。
func probeWritable(dir string) error {
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".codegate-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// probeDatabase 检查数据库文件是否可打开。
func probeDatabase(path string) (error, string) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 不存在是**正常**的：第一次 serve 时会创建。
			return nil, "尚不存在（首次 serve 时创建）"
		}
		return err, path
	}
	info, err := os.Stat(path)
	if err != nil {
		return err, path
	}
	if info.IsDir() {
		return errors.New("路径是一个目录，不是文件"), path
	}
	return nil, fmt.Sprintf("%s（%.1f MB）", path, float64(info.Size())/(1<<20))
}

// probeListen 检查监听地址能否绑定。
//
// ★ 检查完立刻释放。这不是「占住端口」，只是回答
// 「这个地址现在能不能 bind」—— 真正的绑定发生在 serve 里。
func probeListen(addr string) error {
	if addr == "" {
		return errors.New("监听地址为空")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}
