// Package agent 实现 CodeGate Agent：本机的执行边界。
//
// 设计文档：docs/PHASE0-ARCHITECTURE.md §3
//
// # 职责边界（§3.4 代码评审红线）
//
//   - 只做「把本机 PTY 的字节搬给 Server，把 Server 的字节搬给 PTY」
//   - 不解析、不过滤、不改写任何 ANSI/OSC 序列
//   - 不做输出嗅探（`strings.Contains(output, "...")`）
//   - 不记录 stdin 内容到日志，不把 stdout 写日志文件
//   - 不把本机环境变量整体上报 Server
//
// # 为什么它不需要公网 IP
//
// Agent **只出站连接**（§1.3）：它在家里/公司的机器上主动连 Server，
// 于是 NAT 和防火墙都不是问题。这也是整个项目不需要 P2P/内网穿透的原因。
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 默认值
// ---------------------------------------------------------------------------

const (
	// DefaultHeartbeatInterval 是 Agent → Server 的心跳周期（§3.3）。
	DefaultHeartbeatInterval = 20 * time.Second

	// DefaultReconnectMin / Max 是重连退避的上下限（§3.2）。
	//
	// 上限 30s 是有意的：Server 重启通常几秒就绪，但如果是计划内维护，
	// 每分钟两次的探测频率既不会打爆刚起来的 Server，也不会让用户
	// 等太久才发现恢复了。
	DefaultReconnectMin = 1 * time.Second
	DefaultReconnectMax = 30 * time.Second

	// DefaultMaxSessions 是单 Agent 的并发会话上限。
	DefaultMaxSessions = 20

	// DefaultMaxFrameSize 是单个二进制帧的 payload 上限。
	DefaultMaxFrameSize = 1 << 20 // 1 MB

	// DefaultConnectTimeout 是「DNS + TCP + TLS + WS Upgrade」的总超时（§3.2）。
	DefaultConnectTimeout = 15 * time.Second

	// DefaultAuthTimeout 是 hello → challenge → auth → ready 的总超时（§3.2）。
	DefaultAuthTimeout = 10 * time.Second
)

// ---------------------------------------------------------------------------
// 命令白名单
// ---------------------------------------------------------------------------

// CommandKind 是命令的类别，决定前端如何呈现控制动作。
//
// ★ 这个字段是 Phase 2 实测的直接产物（架构文档 §6.5）：
//
//	Windows 上「写 0x03 表示 Ctrl+C」的效果**取决于子进程的控制台模式** ——
//	  - TUI（Claude Code / Codex / vim）启动时自己设 raw mode，
//	    0x03 作为字节送达，应用自己解释 → Ctrl+C 可用；
//	  - shell（cmd / PowerShell）处于 cooked mode，0x03 被 conhost 拦截，
//	    结果是子进程 stdin EOF 而命令并不中断 → Ctrl+C 不可用且破坏会话。
//
// 所以 Agent 必须**如实**告诉前端「这个命令是哪一类」，让前端决定
// Ctrl+C 按钮是否可用。这不是 UI 层自作主张，而是把后端的能力边界
// 如实上报 —— 让按钮要么可用且有效，要么明确不可用并给出替代路径。
type CommandKind string

const (
	// KindShell 是行模式的交互式 shell。
	KindShell CommandKind = "shell"
	// KindTUI 是全屏终端应用（自己会设 raw mode）。
	KindTUI CommandKind = "tui"
)

// Valid 判断类别是否是已知值。
func (k CommandKind) Valid() bool {
	return k == KindShell || k == KindTUI
}

// Interruptible 表示该类命令能否可靠接收 Ctrl+C。
//
// 前端据此决定是否启用 Ctrl+C 按钮。在非 Windows 平台上这个判断同样成立
// （Unix 用真实信号，两类都能中断），所以它表达的是「在当前实现下是否可靠」，
// 由调用方结合平台决定 —— 这里只给出**保守**答案。
func (k CommandKind) Interruptible() bool { return k == KindTUI }

// CommandSpec 是白名单里的一条命令。
//
// 用户永远看不到裸的 `Command`/`Args` —— 界面只显示 `Label`，
// 点一下就按这里定义的参数启动。这是 §21.1「白名单制」的落地方式：
// **浏览器发过来的只有 ID，没有命令行**。
type CommandSpec struct {
	ID      string      `json:"id"`
	Label   string      `json:"label"`
	Command string      `json:"command"`
	Args    []string    `json:"args,omitempty"`
	Kind    CommandKind `json:"kind"`
	// ResumeArgs 是「恢复上次对话」时追加的参数（§21.3）。
	//
	// 例：Claude Code 的 `--continue`。CodeGate 不知道对话内容，
	// 也不知道对话存在哪 —— 它只知道这个 CLI 支持一个 resume 标志。
	// 这是刻意的：一旦开始理解"对话"，这个项目就变成 AI Agent 了。
	ResumeArgs []string `json:"resume_args,omitempty"`
	// WebURL links to a separately hosted HTTPS UI for the same tool.
	WebURL string `json:"web_url,omitempty"`
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// Duration 是能在 JSON 里写成 "20s" 的时长。
//
// 用自定义类型而不是 time.Duration：后者在 JSON 里是纳秒整数，
// 配置文件写成 `"heartbeat_interval": 20000000000` 没人看得懂，
// 也没法一眼核对。
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// 也接受裸数字（纳秒），方便程序生成配置。
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("agent: 时长必须是 \"20s\" 这样的字符串或纳秒整数: %w", err)
		}
		*d = Duration(n)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("agent: 解析时长 %q 失败: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Config 是 Agent 的运行配置。
//
// 所有字段都可以从配置文件（JSON）读，环境变量可覆盖其中一部分
// （见 ApplyEnv）—— 环境变量优先，便于容器化部署。
type Config struct {
	// ---- 连接 ----

	// ServerURL 是 Agent 要连的地址，形如 wss://codegate.example.com/ws/agent。
	ServerURL string `json:"server_url"`

	// Insecure 允许 ws:// 与自签证书。
	//
	// 仅供开发。生产必须 wss —— 终端流量是明文的，
	// 没有 TLS 等于把 shell 会话挂在公网上。
	Insecure bool `json:"insecure,omitempty"`

	// ---- 设备身份 ----

	// DeviceName 是显示在设备列表里的名字。留空则用主机名。
	DeviceName string `json:"device_name,omitempty"`

	// StateDir 存放 device.key / device.json。
	// 留空则用平台默认路径（见 DefaultStateDir）。
	StateDir string `json:"state_dir,omitempty"`

	// ---- 安全边界 ----

	// AllowedRoots 是允许作为工作目录的根路径白名单。
	//
	// ★ 这是**唯一**的路径校验依据，PTY 的 cwd 和文件 API 共用同一份
	// （架构 §6.5 / 文件传输文档 F9）。两处各写一份必然漂移，
	// 而漂移出来的那个就是绕过口。
	AllowedRoots []string `json:"allowed_roots"`

	// AllowedCommands 是预置命令白名单。
	AllowedCommands []CommandSpec `json:"allowed_commands"`

	// AllowCustomCommands 允许浏览器下发任意命令行。
	//
	// ★ 默认 false，且建议保持 false（§21.1）。
	// 打开它等于把「能在这台机器上执行任意命令」的能力交给一个网页，
	// 而网页可能被 XSS 拿到。这比 PTY 本身的风险大得多 ——
	// 用户在终端里敲什么是他自己的选择，而"网页让它敲什么"不是。
	AllowCustomCommands bool `json:"allow_custom_commands"`

	// Env 是额外注入给子进程的环境变量。
	//
	// 只在确实需要时使用（比如某个 CLI 要读一个特定的配置目录变量）。
	// ★ 不要在这里放凭据 —— 远程会话里的程序能看到这些值。
	// 基础环境由 BuildEnv 白名单构造，这里只做补充和覆盖。
	Env map[string]string `json:"env,omitempty"`

	// ---- 资源限制 ----

	MaxSessions  int `json:"max_sessions,omitempty"`
	BufferSize   int `json:"buffer_size,omitempty"`
	MaxFrameSize int `json:"max_frame_size,omitempty"`

	// ---- 心跳与重连 ----

	HeartbeatInterval Duration `json:"heartbeat_interval,omitempty"`
	ReconnectMin      Duration `json:"reconnect_min,omitempty"`
	ReconnectMax      Duration `json:"reconnect_max,omitempty"`

	// UpdateManifestURL points to a trusted HTTPS manifest for optional idle updates.
	UpdateEnabled     bool `json:"update_enabled,omitempty"`
	UpdateInterval    Duration `json:"update_interval,omitempty"`
}

// DefaultConfig 返回一份可用的最小配置。
//
// 刻意**不**填 AllowedRoots 和 AllowedCommands —— 这两项是安全边界，
// 必须由用户显式声明。给一个"看起来能用"的默认值（比如 `C:\`）
// 是这类工具最典型的自毁方式。
func DefaultConfig() Config {
	return Config{
		DeviceName:          "",
		MaxSessions:         DefaultMaxSessions,
		MaxFrameSize:        DefaultMaxFrameSize,
		HeartbeatInterval:   Duration(DefaultHeartbeatInterval),
		ReconnectMin:        Duration(DefaultReconnectMin),
		ReconnectMax:        Duration(DefaultReconnectMax),
		AllowCustomCommands: false,
		UpdateInterval: Duration(time.Hour),
	}
}

// withDefaults 补齐零值。不修改 AllowedRoots / AllowedCommands。
func (c Config) withDefaults() Config {
	d := DefaultConfig()

	if c.MaxSessions <= 0 {
		c.MaxSessions = d.MaxSessions
	}
	if c.MaxFrameSize <= 0 {
		c.MaxFrameSize = d.MaxFrameSize
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = d.HeartbeatInterval
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = d.ReconnectMin
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = d.ReconnectMax
	}
	// 退避上限不能小于下限，否则退避逻辑会算出递减的间隔。
	if c.ReconnectMax < c.ReconnectMin {
		c.ReconnectMax = c.ReconnectMin
	}
	if c.UpdateInterval <= 0 {
		c.UpdateInterval = d.UpdateInterval
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	return c
}

// Validate 检查配置是否可用。
//
// ★ 只做**结构性**校验（必填、格式、唯一性），不碰文件系统 ——
// 校验失败要能在没有磁盘权限的环境里复现。路径是否存在、
// 是否可写由 Prepare 负责。
func (c Config) Validate() error {
	if strings.TrimSpace(c.ServerURL) == "" {
		return errors.New("agent: server_url 不能为空")
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil {
		return fmt.Errorf("agent: server_url 解析失败: %w", err)
	}
	switch u.Scheme {
	case "wss", "https":
		// 正常。
	case "ws", "http":
		if !c.Insecure {
			return fmt.Errorf(
				"agent: server_url 用了不加密的 %s://，这会明文传输终端内容。"+
					"要么改用 wss://，要么显式设置 insecure=true（仅限开发）", u.Scheme)
		}
	default:
		return fmt.Errorf("agent: server_url 的协议 %q 不支持（应为 wss:// 或 ws://）", u.Scheme)
	}

	if c.UpdateEnabled {
		if u.Scheme != "wss" && u.Scheme != "https" {
			return errors.New("agent: 启用更新需要加密的 Server 连接")
		}
		if c.UpdateInterval.Std() < time.Minute {
			return errors.New("agent: update_interval 不能短于 1m")
		}
	}

	if len(c.AllowedRoots) == 0 {
		return errors.New("agent: allowed_roots 不能为空 —— " +
			"必须显式声明哪些目录可以被远程访问，不能默认为整个磁盘")
	}
	for i, r := range c.AllowedRoots {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("agent: allowed_roots[%d] 为空", i)
		}
		if !filepath.IsAbs(r) {
			return fmt.Errorf("agent: allowed_roots[%d] %q 必须是绝对路径", i, r)
		}
	}

	if len(c.AllowedCommands) == 0 && !c.AllowCustomCommands {
		return errors.New("agent: allowed_commands 为空且未开启 allow_custom_commands —— " +
			"没有任何可执行的命令")
	}

	seen := make(map[string]bool, len(c.AllowedCommands))
	for i, cmd := range c.AllowedCommands {
		if strings.TrimSpace(cmd.ID) == "" {
			return fmt.Errorf("agent: allowed_commands[%d] 缺少 id", i)
		}
		if seen[cmd.ID] {
			return fmt.Errorf("agent: allowed_commands 里 id %q 重复", cmd.ID)
		}
		seen[cmd.ID] = true

		if strings.TrimSpace(cmd.Command) == "" {
			return fmt.Errorf("agent: allowed_commands[%d] (%s) 缺少 command", i, cmd.ID)
		}
		if cmd.WebURL != "" {
			web, err := url.Parse(cmd.WebURL)
			if err != nil || web.Scheme != "https" || web.Host == "" || web.User != nil || web.Fragment != "" {
				return fmt.Errorf("agent: allowed_commands[%d] (%s) 的 web_url 必须是 HTTPS URL", i, cmd.ID)
			}
		}
		if !cmd.Kind.Valid() {
			return fmt.Errorf(
				"agent: allowed_commands[%d] (%s) 的 kind %q 非法。"+
					"必须是 %q 或 %q —— 它决定前端能否提供 Ctrl+C（架构 §6.5）",
				i, cmd.ID, cmd.Kind, KindShell, KindTUI)
		}
	}

	if c.HeartbeatInterval.Std() <= 0 {
		return errors.New("agent: heartbeat_interval 必须为正")
	}
	if c.MaxSessions <= 0 {
		return errors.New("agent: max_sessions 必须为正")
	}
	return nil
}

// Prepare 校验并把配置规整成可直接使用的形态。
//
// 返回的 Config 已补齐默认值，AllowedRoots 已转成绝对路径并去重。
// 这是 Agent 启动时唯一该调用的入口。
func (c Config) Prepare() (Config, error) {
	c = c.withDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}

	roots := make([]string, 0, len(c.AllowedRoots))
	seen := make(map[string]bool, len(c.AllowedRoots))
	for _, r := range c.AllowedRoots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return Config{}, fmt.Errorf("agent: allowed_roots 里的 %q 无法转成绝对路径: %w", r, err)
		}
		// 大小写不敏感的平台（Windows）要去重时忽略大小写，
		// 否则 `C:\Work` 和 `c:\work` 会被当成两个根，白名单形同虚设。
		key := abs
		if isCaseInsensitiveFS() {
			key = strings.ToLower(abs)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		roots = append(roots, abs)
	}
	c.AllowedRoots = roots

	// 归一化白名单命令的可执行文件路径。
	//
	// ★ 放在 Prepare 而不是 ResolveCommand：这里改的是**配置本身**，
	// 于是 `agent config` / `agent doctor` 显示的就是真正会被执行的那个路径。
	// 只在执行时改的话，配置文件和实际行为会长期不一致 ——
	// 而排查这类问题时，人第一眼看的就是配置文件。
	//
	// 先复制再改：CommandSpec 里含切片，直接改会动到调用方的底层数组。
	cmds := make([]CommandSpec, len(c.AllowedCommands))
	copy(cmds, c.AllowedCommands)
	for i := range cmds {
		cmds[i].Command = normalizeCommandPath(cmds[i].Command)
	}
	c.AllowedCommands = cmds

	if c.DeviceName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.DeviceName = h
		} else {
			c.DeviceName = "codegate-agent"
		}
	}
	return c, nil
}

// FindCommand 按 ID 查白名单。
func (c Config) FindCommand(id string) (CommandSpec, bool) {
	for _, cmd := range c.AllowedCommands {
		if cmd.ID == id {
			return cmd, true
		}
	}
	return CommandSpec{}, false
}

// Load 从 JSON 文件读配置。
//
// 文件不存在时返回 DefaultConfig 而不是错误 —— 首次运行本来就没有配置，
// 让调用方去区分"没有配置"和"配置坏了"只会制造麻烦。
// 但**配置存在却解析失败**必须报错：那说明用户写了东西但写错了，
// 静默退回默认值会让他以为配置生效了。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("agent: 读取配置 %s 失败: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("agent: 解析配置 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// ApplyEnv 用环境变量覆盖配置。
//
// 只覆盖「部署时确实会变」的几项，不做一个通用的
// `CODEGATE_AGENT_* → 字段` 反射映射 —— 那种设计会让配置文件
// 和环境变量之间的关系变得不可预测，排查问题时无从下手。
func (c *Config) ApplyEnv(getenv func(string) string) {
	if v := getenv("CODEGATE_SERVER_URL"); v != "" {
		c.ServerURL = v
	}
	if v := getenv("CODEGATE_DEVICE_NAME"); v != "" {
		c.DeviceName = v
	}
	if v := getenv("CODEGATE_STATE_DIR"); v != "" {
		c.StateDir = v
	}
	if v := getenv("CODEGATE_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
		c.Insecure = true
	}
}

// DefaultStateDir 返回平台默认的状态目录。
//
// 放用户目录而不是程序目录：device.key 是这台设备的身份，
// 它应该跟着用户走（重装程序不丢），也不该被写进可能有同步/备份
// 的仓库目录里。
func DefaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "codegate")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".codegate")
	}
	return ".codegate"
}
