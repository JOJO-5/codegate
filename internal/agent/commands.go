package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// 命令解析的哨兵错误。
var (
	// ErrCommandNotAllowed 表示请求的命令不在白名单内（或自定义命令被禁用）。
	ErrCommandNotAllowed = errors.New("agent: 命令不在白名单内")
	// ErrCommandAmbiguous 表示请求里同时给了 command_id 和 command。
	ErrCommandAmbiguous = errors.New("agent: 不能同时指定 command_id 和 command")
)

// ResolvedCommand 是最终要执行的具体命令。
type ResolvedCommand struct {
	Command string
	Args    []string
	Kind    CommandKind
	Label   string
	// Custom 为 true 表示它来自浏览器的自定义输入（allow_custom_commands）。
	Custom bool
}

// ResolveCommand 把浏览器发来的创建请求解析成具体命令。
//
// ★ 安全要点：**浏览器只能给 ID，不能给命令行**（除非显式开启
// allow_custom_commands）。所有参数都来自本地配置文件，请求里的字段
// 只是「选哪一个」，不是「怎么执行」。
//
// 这个区分是整个授权模型的支点：它意味着「被 XSS 拿到的浏览器会话」
// 最多只能在预置的几条命令里挑一条，而不是往用户机器上执行任意代码。
func (c Config) ResolveCommand(commandID, command string, args []string, resume bool) (ResolvedCommand, error) {
	switch {
	case commandID != "":
		// 同时给了 ID 和裸命令 —— 拒绝而不是"以 ID 为准"。
		// 混用说明调用方对协议的理解有偏差，此时静默忽略一半输入
		// 会让问题潜伏到更难查的地方。
		if command != "" || len(args) > 0 {
			return ResolvedCommand{}, ErrCommandAmbiguous
		}

		spec, ok := c.FindCommand(commandID)
		if !ok {
			return ResolvedCommand{}, fmt.Errorf("%w: %s", ErrCommandNotAllowed, commandID)
		}

		out := ResolvedCommand{
			Command: spec.Command,
			Args:    append([]string(nil), spec.Args...),
			Kind:    spec.Kind,
			Label:   spec.Label,
		}
		if resume {
			out.Args = append(out.Args, spec.ResumeArgs...)
		}
		return out, nil

	case command != "":
		if !c.AllowCustomCommands {
			return ResolvedCommand{}, fmt.Errorf(
				"%w: 自定义命令已禁用（allow_custom_commands=false）", ErrCommandNotAllowed)
		}
		// ★ 自定义命令的 kind 一律按 shell 处理。
		//
		// 我们无法可靠地判断一个任意命令行是不是 TUI（那需要解析它、
		// 甚至运行它），所以取保守值：前端会把 Ctrl+C 置灰，
		// 用户改用白名单条目就能拿到更好的交互。
		// 猜错方向的代价不对称 —— 把 shell 当 TUI 会让用户点一个
		// 会破坏会话的按钮，反过来只是少一个功能。
		// 自定义命令不走 Prepare（它不来自配置文件），所以在这里归一化。
		// 同一个坑：`C:/Windows/System32/cmd.exe /c ...` 会让 cmd.exe
		// 把路径里的 `/c` 当开关。见 normalizeCommandPath。
		return ResolvedCommand{
			Command: normalizeCommandPath(command),
			Args:    append([]string(nil), args...),
			Kind:    KindShell,
			Label:   filepath.Base(command),
			Custom:  true,
		}, nil

	default:
		return ResolvedCommand{}, errors.New("agent: 请求里既没有 command_id 也没有 command")
	}
}

// ---------------------------------------------------------------------------
// 环境变量构造
// ---------------------------------------------------------------------------

// passthroughEnv 是允许从 Agent 自身环境透传给子进程的变量。
//
// ★ 这是一个**白名单**，不是黑名单。差别很关键：黑名单要穷举
// "哪些变量不该传"，而系统里有无数个我们想不到的变量（各种 SDK、
// CI、企业内部工具注入的），漏掉一个就泄露一次。
//
// 这里保留的都是「进程能不能正常跑起来」所必需的：可执行文件查找路径、
// 系统目录、临时目录、用户标识。凡是与"跑起来"无关的（代理、凭据、
// 各种 API key）一律不带。
var passthroughEnv = []string{
	// 通用
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL",
	// Windows
	"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT",
	"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
	"APPDATA", "LOCALAPPDATA", "USERNAME", "USERDOMAIN",
	"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "OS",
}

// BuildEnv 构造子进程的环境变量。
//
// ★★ 绝不整体继承 os.Environ()。这是 F2 的实测教训（架构 §6.2）：
//
//   - **不设 TERM**：Codex 会直接拒绝进入 TUI —— 输出只有 411 字节，
//     停在 `Continue anyway? [y/N]`；设成 xterm-256color 后才有
//     完整的 3,976 字节 TUI 渲染。
//   - **整体继承**：会把本机坏掉的 https_proxy 一起传下去，
//     Claude Code 报 `Failed to connect to api.anthropic.com: Status 502,
//     A proxy is configured via https_proxy` 就是证据。
//
// 结论：环境必须**显式构造**，既不能少（少了 TERM 就没 TUI），
// 也不能多（多了就泄露本机配置）。
func BuildEnv(cfg Config, cols, rows uint16) []string {
	env := make([]string, 0, len(passthroughEnv)+8+len(cfg.Env))
	seen := make(map[string]bool, len(passthroughEnv))

	// 1. 白名单透传：只带 Agent 环境里确实存在的。
	for _, k := range passthroughEnv {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			continue
		}
		env = append(env, k+"="+v)
		seen[strings.ToUpper(k)] = true
	}

	// 2. 用户自定义（配置文件里的 env）。
	//
	// 优先级高于系统环境（用户配置比"本机碰巧是什么"更可信），
	// 但**低于**下面的强制值 —— 见第 3 步的说明。
	for k, v := range cfg.Env {
		if seen[strings.ToUpper(k)] {
			env = replaceEnv(env, k, v)
			continue
		}
		env = append(env, k+"="+v)
		seen[strings.ToUpper(k)] = true
	}

	// 3. 强制覆盖：这几项**必须**由我们决定，用户配置也不许改。
	//
	// ★ 为什么连用户配置都要挡：这几个不是"偏好"，是 CodeGate 的
	// 实现约束 ——
	//   - TERM：F2 实测证明不给它，Codex 直接拒绝进入 TUI
	//   - LINES/COLUMNS：必须与会话的真实尺寸一致，否则首帧布局就是错的
	// 允许用户改它们，只会制造"配置看起来生效了、但 TUI 起不来"
	// 这类极难归因的问题。用户要加自己的变量，用 cfg.Env 加别的键。
	force := []string{
		// TERM 不给，TUI 直接拒绝启动（F2）。xterm-256color 是覆盖面
		// 最广的选择：几乎所有 CLI 都认它，且支持 256 色。
		"TERM=xterm-256color",
		// COLORTERM 决定"要不要用真彩色"。Claude Code 等会读它。
		"COLORTERM=truecolor",
		// 会话尺寸。Windows 侧真正生效的是 ConPTY 的尺寸设置，
		// 但不少程序（尤其 Unix 侧和跨平台的 TUI）会先读这两个变量，
		// 不一致会导致首帧布局错位。
		"LINES=" + strconv.Itoa(int(rows)),
		"COLUMNS=" + strconv.Itoa(int(cols)),
		// 让子进程知道自己跑在 CodeGate 里，便于 CLI 或用户脚本做区分。
		"CODEGATE=1",
	}
	for _, kv := range force {
		k, v, _ := strings.Cut(kv, "=")
		env = replaceEnv(env, k, v)
	}

	return env
}

// replaceEnv 设置或追加一个环境变量（大小写不敏感地查找键）。
//
// Windows 的环境变量名不区分大小写，`Path` 和 `PATH` 是同一个变量。
// 直接 append 会产生两条同名记录，而 Windows 取哪一条是未定义的 ——
// 表现为"有时候 Path 生效有时候不生效"这种极难复现的问题。
func replaceEnv(env []string, key, value string) []string {
	want := strings.ToUpper(key)
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.ToUpper(k) == want {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

// DescribePlatform 返回用于 agent.hello 的平台与架构标识。
func DescribePlatform() (platform, arch string) {
	return runtime.GOOS, runtime.GOARCH
}
