// Package config 集中管理 Server 与 Agent 的配置：默认值、环境变量覆盖、校验。
//
// 设计原则：**配置项必须有默认值**，除了少数无法安全猜测的（JWT secret、DB 路径）。
// 部署时只覆盖真正需要变的项，而不是被迫写满一屏。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TLSConfig 是 HTTPS 相关配置。
//
// 生产环境更常见的是「反代终止 TLS」，此时 Enabled=false，由 BaseURL 的 scheme
// 表达对外协议。两者都支持，不做互斥校验。
type TLSConfig struct {
	Enabled  bool
	CertFile string
	KeyFile  string
}

// Config 是 Server 的运行配置。
type Config struct {
	// ---- 网络 ----
	Listen  string // 形如 ":8080" 或 "127.0.0.1:8080"
	BaseURL string // 对外地址，用于 Origin 校验与 cookie 作用域。为空则按请求 Host 推断

	TLS TLSConfig

	// ---- 存储 ----
	DBPath string

	// ---- 认证 ----
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// AllowSignup 控制 /auth/register 是否开放。
	// 注意：系统内一个用户都没有时，无论此值为假都允许注册第一个用户（引导）。
	AllowSignup bool
	// AllowedOrigins 是浏览器 WS 连接允许的 Origin 白名单。
	// 空列表 = 不校验 Origin（仅开发用，启动时会记警告）。
	AllowedOrigins []string

	// ---- Pairing ----
	PairingCodeTTL time.Duration

	// ---- 下发给 Agent 的资源上限（§35）----
	HeartbeatInterval time.Duration
	MaxSessions       int
	MaxFrameSize      int
	MaxBufferSize     int
	MaxMessageSize    int

	// ---- 连接 ----
	SendQueueSize int // 每条连接的发送队列深度（R6：不要给到 1024）
	WriteTimeout  time.Duration
	PongTimeout   time.Duration // 超过这个时间没收到 Pong/心跳 → 判定连接死亡

	// ---- 反向代理 ----
	// TrustedProxies 为空时**不信任**任何 X-Forwarded-For，
	// 直接取 RemoteAddr。否则攻击者可以伪造 IP 绕过限流与审计。
	TrustedProxies []string

	// ---- 可观测 ----
	LogLevel string // debug | info | warn | error

	warnings []string
}

// Warnings 返回加载过程中产生的非致命问题（例如自动生成了 JWT secret）。
// 由 main 在日志初始化后打印 —— 不能在这里打，因为日志还没配好。
func (c *Config) Warnings() []string { return c.warnings }

// Defaults 返回一份带默认值的配置。
//
// 默认值的选择标准：**在一台干净的开发机上能直接 `codegate-server serve` 跑起来**。
func Defaults() *Config {
	return &Config{
		Listen:            ":8080",
		DBPath:            DefaultDBPath(),
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   30 * 24 * time.Hour,
		AllowSignup:       false,
		PairingCodeTTL:    10 * time.Minute,
		HeartbeatInterval: 20 * time.Second,
		MaxSessions:       20,
		MaxFrameSize:      64 * 1024,
		MaxBufferSize:     1024 * 1024,
		MaxMessageSize:    64 * 1024,
		SendQueueSize:     256,
		WriteTimeout:      10 * time.Second,
		PongTimeout:       60 * time.Second,
		LogLevel:          "info",
	}
}

// Load 读取环境变量并合并到默认配置上。
//
// 环境变量一律 `CODEGATE_` 前缀。返回的 error 只用于「值本身非法」，
// 缺少必需项由 Validate 统一报（这样错误信息能一次给全）。
func Load() (*Config, error) {
	c := Defaults()
	if err := c.applyEnv(os.Getenv); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv 把环境变量合并进配置。getenv 参数化是为了可测。
func (c *Config) applyEnv(getenv func(string) string) error {
	if v := getenv("CODEGATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("CODEGATE_BASE_URL"); v != "" {
		c.BaseURL = strings.TrimRight(v, "/")
	}
	if v := getenv("CODEGATE_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := getenv("CODEGATE_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}

	// TLS：支持从环境变量直接给证书路径。
	if v := getenv("CODEGATE_TLS_CERT"); v != "" {
		c.TLS.CertFile = v
	}
	if v := getenv("CODEGATE_TLS_KEY"); v != "" {
		c.TLS.KeyFile = v
	}
	c.TLS.Enabled = c.TLS.CertFile != "" && c.TLS.KeyFile != ""

	// JWT secret：允许直接给，也允许从文件读（推荐 —— 避免出现在进程环境里）。
	// secret 一旦泄露，攻击者可以伪造任意用户的 access token。
	switch {
	case getenv("CODEGATE_JWT_SECRET") != "":
		c.JWTSecret = []byte(getenv("CODEGATE_JWT_SECRET"))
	case getenv("CODEGATE_JWT_SECRET_FILE") != "":
		path := getenv("CODEGATE_JWT_SECRET_FILE")
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("config: 读取 JWT secret 文件失败: %w", err)
		}
		c.JWTSecret = []byte(strings.TrimSpace(string(raw)))
	}

	if err := c.applyEnvDurations(getenv); err != nil {
		return err
	}
	if err := c.applyEnvInts(getenv); err != nil {
		return err
	}

	if v := getenv("CODEGATE_ALLOWED_ORIGINS"); v != "" {
		c.AllowedOrigins = splitTrim(v)
	}
	if v := getenv("CODEGATE_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = splitTrim(v)
	}
	if v := getenv("CODEGATE_ALLOW_SIGNUP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: CODEGATE_ALLOW_SIGNUP 不是合法布尔值: %q", v)
		}
		c.AllowSignup = b
	}

	// secret 缺失时自动生成一个并记警告 —— 否则开发机上根本起不来。
	// 但每次重启都会换 secret，导致已签发的 access token 全部失效（15 分钟内）。
	// 这个代价在开发环境可以接受，生产必须显式配置。
	if len(c.JWTSecret) == 0 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return fmt.Errorf("config: 生成临时 JWT secret 失败: %w", err)
		}
		c.JWTSecret = []byte(base64.RawURLEncoding.EncodeToString(secret))
		c.warnings = append(c.warnings,
			"未配置 CODEGATE_JWT_SECRET(_FILE)，已生成临时密钥；"+
				"重启后所有 access token 失效。生产环境必须显式配置。")
	}
	return nil
}

func (c *Config) applyEnvDurations(getenv func(string) string) error {
	for _, it := range []struct {
		key string
		dst *time.Duration
	}{
		{"CODEGATE_ACCESS_TOKEN_TTL", &c.AccessTokenTTL},
		{"CODEGATE_REFRESH_TOKEN_TTL", &c.RefreshTokenTTL},
		{"CODEGATE_PAIRING_CODE_TTL", &c.PairingCodeTTL},
		{"CODEGATE_HEARTBEAT_INTERVAL", &c.HeartbeatInterval},
		{"CODEGATE_WRITE_TIMEOUT", &c.WriteTimeout},
		{"CODEGATE_PONG_TIMEOUT", &c.PongTimeout},
	} {
		v := getenv(it.key)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: %s 不是合法时长（如 15m / 30s）: %q", it.key, v)
		}
		*it.dst = d
	}
	return nil
}

func (c *Config) applyEnvInts(getenv func(string) string) error {
	for _, it := range []struct {
		key string
		dst *int
	}{
		{"CODEGATE_MAX_SESSIONS", &c.MaxSessions},
		{"CODEGATE_MAX_FRAME_SIZE", &c.MaxFrameSize},
		{"CODEGATE_MAX_BUFFER_SIZE", &c.MaxBufferSize},
		{"CODEGATE_MAX_MESSAGE_SIZE", &c.MaxMessageSize},
		{"CODEGATE_SEND_QUEUE_SIZE", &c.SendQueueSize},
	} {
		v := getenv(it.key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: %s 不是合法整数: %q", it.key, v)
		}
		*it.dst = n
	}
	return nil
}

// Validate 校验配置的完整性与自洽性。一次报出所有问题，不要让用户改一个跑一次。
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Listen) == "" {
		problems = append(problems, "listen 地址为空")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		problems = append(problems, "db_path 为空")
	}
	// JWT secret 太短等于没有 secret：HS256 的密钥强度直接决定 token 不可伪造性。
	if len(c.JWTSecret) < 32 {
		problems = append(problems, fmt.Sprintf(
			"jwt secret 只有 %d 字节，至少需要 32 字节", len(c.JWTSecret)))
	}
	if c.AccessTokenTTL <= 0 {
		problems = append(problems, "access_token_ttl 必须为正")
	}
	if c.RefreshTokenTTL <= c.AccessTokenTTL {
		problems = append(problems, "refresh_token_ttl 必须大于 access_token_ttl")
	}
	if c.PairingCodeTTL <= 0 {
		problems = append(problems, "pairing_code_ttl 必须为正")
	}
	if c.HeartbeatInterval <= 0 {
		problems = append(problems, "heartbeat_interval 必须为正")
	}
	if c.PongTimeout <= c.HeartbeatInterval {
		// 否则 Agent 还没来得及发下一次心跳就被判死，会陷入「连上就掉」的循环。
		problems = append(problems, "pong_timeout 必须大于 heartbeat_interval")
	}
	if c.MaxSessions <= 0 {
		problems = append(problems, "max_sessions 必须为正")
	}
	if c.MaxFrameSize < 1024 {
		// 太小的 frame 上限会让正常终端输出被拒，属于配置事故。
		problems = append(problems, "max_frame_size 至少 1024")
	}
	if c.MaxMessageSize <= 0 {
		problems = append(problems, "max_message_size 必须为正")
	}
	if c.SendQueueSize <= 0 {
		problems = append(problems, "send_queue_size 必须为正")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			problems = append(problems, fmt.Sprintf("base_url 不是合法 http(s) 地址: %q", c.BaseURL))
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("log_level 非法: %q（可选 debug|info|warn|error）", c.LogLevel))
	}
	if c.TLS.Enabled && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		problems = append(problems, "tls 已启用但证书或私钥路径为空")
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: 配置有 %d 个问题:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// SecureCookies 判断 refresh token cookie 是否该带 Secure 属性。
//
// 判定依据是对外协议而不是本地监听方式：反代终止 TLS 时本地是 http，
// 但浏览器看到的是 https，此时 cookie 仍必须是 Secure。
func (c *Config) SecureCookies() bool {
	if c.BaseURL != "" {
		return strings.HasPrefix(c.BaseURL, "https://")
	}
	return c.TLS.Enabled
}

// OriginAllowed 判断一个 Origin 头是否在白名单内。
//
// 白名单为空时返回 true（开发模式），调用方负责在启动时警告。
// 比较时忽略大小写与末尾斜杠，避免因书写差异误判。
func (c *Config) OriginAllowed(origin string) bool {
	if len(c.AllowedOrigins) == 0 {
		return true
	}
	if origin == "" {
		// 非浏览器客户端（如测试工具）不带 Origin。
		// 它们没有 cookie 可被 CSRF 利用，放行是安全的。
		return true
	}
	norm := strings.ToLower(strings.TrimRight(origin, "/"))
	for _, a := range c.AllowedOrigins {
		if strings.ToLower(strings.TrimRight(a, "/")) == norm {
			return true
		}
	}
	return false
}

// splitTrim 按逗号切分并去掉空白项。
func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
