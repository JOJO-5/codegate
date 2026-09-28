package agent

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestResolveCommandByID(t *testing.T) {
	c := validConfig(t)

	got, err := c.ResolveCommand("claude", "", nil, false)
	if err != nil {
		t.Fatalf("解析白名单命令失败: %v", err)
	}
	if got.Command != "claude" || got.Kind != KindTUI {
		t.Errorf("解析结果 = %+v", got)
	}
	if got.Custom {
		t.Error("白名单命令不该被标记为 Custom")
	}
}

// TestResolveCommandResumeAppendsArgs 验证「恢复上次对话」（§21.3）。
//
// CodeGate 不知道对话内容，只知道这个 CLI 支持 resume 标志 ——
// 所以它只是往 argv 后面追加配置里声明好的参数。
func TestResolveCommandResumeAppendsArgs(t *testing.T) {
	c := validConfig(t)

	base, err := c.ResolveCommand("claude", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.ResolveCommand("claude", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(resumed.Args) != len(base.Args)+len(c.AllowedCommands[1].ResumeArgs) {
		t.Fatalf("resume 后参数数量不对: %v", resumed.Args)
	}
	if !strings.Contains(strings.Join(resumed.Args, " "), "--continue") {
		t.Errorf("resume 参数没有被追加: %v", resumed.Args)
	}
}

func TestResolveCommandRejectsUnknownID(t *testing.T) {
	c := validConfig(t)

	if _, err := c.ResolveCommand("does-not-exist", "", nil, false); !errors.Is(err, ErrCommandNotAllowed) {
		t.Errorf("未知命令 ID 没有被拒（err=%v）", err)
	}
}

// TestResolveCommandRejectsAmbiguous 验证「同时给 ID 和裸命令」被拒绝。
//
// 静默以 ID 为准会让调用方对协议的错误理解潜伏下去，
// 直到某天有人以为"我传的命令生效了"。
func TestResolveCommandRejectsAmbiguous(t *testing.T) {
	c := validConfig(t)

	if _, err := c.ResolveCommand("claude", "rm", []string{"-rf", "/"}, false); !errors.Is(err, ErrCommandAmbiguous) {
		t.Errorf("歧义请求没有被拒（err=%v）", err)
	}
}

func TestResolveCommandCustomDisabledByDefault(t *testing.T) {
	c := validConfig(t)
	if c.AllowCustomCommands {
		t.Fatal("测试前提错误：默认配置不该开启自定义命令")
	}

	if _, err := c.ResolveCommand("", "rm", []string{"-rf", "/"}, false); !errors.Is(err, ErrCommandNotAllowed) {
		t.Errorf("默认配置下自定义命令没有被拒（err=%v）", err)
	}
}

func TestResolveCommandCustomWhenEnabled(t *testing.T) {
	c := validConfig(t)
	c.AllowCustomCommands = true

	got, err := c.ResolveCommand("", "my-tool", []string{"--flag"}, false)
	if err != nil {
		t.Fatalf("开启后自定义命令仍被拒: %v", err)
	}
	if !got.Custom {
		t.Error("自定义命令没有被标记为 Custom")
	}
	if got.Kind != KindShell {
		t.Errorf("自定义命令的 kind = %q，期望保守值 %q —— "+
			"猜成 TUI 会让前端给出一个会破坏会话的 Ctrl+C 按钮",
			got.Kind, KindShell)
	}
}

func TestResolveCommandEmpty(t *testing.T) {
	c := validConfig(t)
	if _, err := c.ResolveCommand("", "", nil, false); err == nil {
		t.Error("既没有 ID 也没有命令的请求被接受了")
	}
}

// ---------------------------------------------------------------------------
// 命令路径归一化（cmd.exe 的 /c 被路径抢走）
// ---------------------------------------------------------------------------

// TestNormalizeCommandPath 覆盖一个实测出来的 Windows 坑。
//
// 现象：配了 `C:/Windows/System32/cmd.exe` 的会话，终端里只有一句
// 「命令语法不正确。」，和用户配置的命令毫无字面关系。
//
// 根因：cmd.exe 用**字符串扫描**找自己的 /c 开关，而不是先剥掉 argv[0]。
// 于是路径里的 `/cmd.exe` 先命中 —— 它把 `/cmd.exe` 读成「开关 /c +
// 尾巴 md.exe」，剩下的 `md.exe /c echo hi` 成了要执行的命令。
// 而 `md` 恰好是 cmd 的内置命令（mkdir 的别名），`/c` 对它不是合法路径
// 参数，于是报语法错误。
//
// 实测（cmd/conpty-diag，Windows 11）：
//
//	C:\Windows\System32\cmd.exe /c echo X  → 打印 X            ✅
//	C:/Windows/System32/cmd.exe /c echo X  → 命令语法不正确。   ❌
//	裸 cmd /c echo X                        → 打印 X            ✅
//
// Windows 上 `/` 与 `\` 对文件系统等价，所以直接归一化。
func TestNormalizeCommandPath(t *testing.T) {
	cases := []struct {
		in      string
		wantWin string
	}{
		{`C:/Windows/System32/cmd.exe`, `C:\Windows\System32\cmd.exe`},
		{`C:\Windows\System32\cmd.exe`, `C:\Windows\System32\cmd.exe`},
		{`cmd.exe`, `cmd.exe`},
		{`./bin/tool.exe`, `.\bin\tool.exe`},
		{``, ``},
	}

	for _, c := range cases {
		want := c.in
		if runtime.GOOS == "windows" {
			want = c.wantWin
		}
		if got := normalizeCommandPath(c.in); got != want {
			t.Errorf("normalizeCommandPath(%q) = %q，期望 %q（GOOS=%s）",
				c.in, got, want, runtime.GOOS)
		}
	}
}

// TestPrepareNormalizesCommandPaths 验证配置文件里写正斜杠也能跑。
//
// 归一化放在 Prepare（而不是只在执行时）是有意的：这样
// `agent config` / `agent doctor` 显示的就是真正会被执行的那个路径。
// 只在执行时改的话，配置文件和实际行为长期不一致 ——
// 而排查这类问题时，人第一眼看的就是配置文件。
func TestPrepareNormalizesCommandPaths(t *testing.T) {
	const original = `C:/Windows/System32/cmd.exe`

	c := validConfig(t)
	c.AllowedCommands[0].Command = original

	got, err := c.Prepare()
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}

	want := original
	if runtime.GOOS == "windows" {
		want = `C:\Windows\System32\cmd.exe`
	}
	if got.AllowedCommands[0].Command != want {
		t.Errorf("Prepare 后的 command = %q，期望 %q", got.AllowedCommands[0].Command, want)
	}

	// ★ 不能改到调用方的底层数组：Prepare 是值接收者，
	// 但它拿到的切片和调用方共享同一块内存。
	if c.AllowedCommands[0].Command != original {
		t.Errorf("Prepare 修改了调用方的 AllowedCommands（现在是 %q）—— "+
			"配置对象被就地改掉会让「谁改的」变得无法追查",
			c.AllowedCommands[0].Command)
	}
}

// TestResolveCommandNormalizesCustomPath 覆盖自定义命令。
//
// 自定义命令不来自配置文件，不走 Prepare，所以 ResolveCommand
// 里必须自己归一化一次 —— 否则只有 allow_custom_commands 这条路
// 还留着那个坑。
func TestResolveCommandNormalizesCustomPath(t *testing.T) {
	c := validConfig(t)
	c.AllowCustomCommands = true

	args := []string{"/c", "echo", "hi"}
	got, err := c.ResolveCommand("", `C:/Windows/System32/cmd.exe`, args, false)
	if err != nil {
		t.Fatalf("解析自定义命令失败: %v", err)
	}

	want := `C:/Windows/System32/cmd.exe`
	if runtime.GOOS == "windows" {
		want = `C:\Windows\System32\cmd.exe`
	}
	if got.Command != want {
		t.Errorf("command = %q，期望 %q", got.Command, want)
	}

	// ★ args 里的正斜杠必须**原样保留** —— `/c` 本身就是开关，
	// 改了它比不改更糟。
	if strings.Join(got.Args, " ") != "/c echo hi" {
		t.Errorf("args 被改动了: %v —— args 里的 / 是开关，不能动", got.Args)
	}
}

// ---------------------------------------------------------------------------
// BuildEnv
// ---------------------------------------------------------------------------

// envValue 从 "K=V" 切片里取值（大小写不敏感，与 Windows 行为一致）。
func envValue(env []string, key string) string {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// TestBuildEnvForcesTerm 是 F2 的回归测试。
//
// 实测：不设 TERM，Codex 会直接拒绝进入 TUI（输出只有 411 字节，
// 停在 `Continue anyway? [y/N]`）；设成 xterm-256color 后才有完整的
// 3,976 字节 TUI。所以 TERM 必须由我们强制设定，且**不能**被
// 父进程环境带偏。
func TestBuildEnvForcesTerm(t *testing.T) {
	t.Setenv("TERM", "dumb") // 父进程环境里是坏的

	env := BuildEnv(DefaultConfig(), 120, 40)

	if got := envValue(env, "TERM"); got != "xterm-256color" {
		t.Errorf("TERM = %q，期望 xterm-256color（F2 的教训）", got)
	}
	if got := envValue(env, "COLORTERM"); got != "truecolor" {
		t.Errorf("COLORTERM = %q，期望 truecolor", got)
	}
	if got := envValue(env, "LINES"); got != "40" {
		t.Errorf("LINES = %q，期望 40", got)
	}
	if got := envValue(env, "COLUMNS"); got != "120" {
		t.Errorf("COLUMNS = %q，期望 120", got)
	}
}

// TestBuildEnvDropsProxy 是 F2 另一半的回归测试。
//
// 实测：本机 https_proxy 指向一个坏代理，Claude Code 报
// `Failed to connect to api.anthropic.com: Status 502,
// A proxy is configured via https_proxy`。
// 整体继承环境会把这类本机配置一起传进远程会话 —— 既不好用，
// 也等于把用户的网络拓扑暴露给了远程界面。
func TestBuildEnvDropsProxy(t *testing.T) {
	t.Setenv("https_proxy", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("http_proxy", "http://127.0.0.1:9")
	t.Setenv("SOME_SECRET_TOKEN", "super-secret-value")

	env := BuildEnv(DefaultConfig(), 80, 24)

	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			t.Errorf("代理变量 %s 被透传给了子进程（值 %q）—— 这正是 F2 的坑", k, v)
		case "SOME_SECRET_TOKEN":
			t.Errorf("不在白名单里的 %s 被透传了（值 %q）", k, v)
		}
	}
}

func TestBuildEnvAppliesUserEnv(t *testing.T) {
	c := DefaultConfig()
	c.Env = map[string]string{"MY_CLI_CONFIG_DIR": "/tmp/cfg"}

	env := BuildEnv(c, 80, 24)

	if got := envValue(env, "MY_CLI_CONFIG_DIR"); got != "/tmp/cfg" {
		t.Errorf("自定义变量没有被注入: %q", got)
	}
}

// TestBuildEnvTerminalVarsAreNotOverridable 说明一个刻意的设计取舍：
// 用户配置**不能**覆盖 TERM / COLORTERM / LINES / COLUMNS / CODEGATE。
//
// 这几个不是"偏好"，是 CodeGate 的实现约束 ——
// F2 实测证明不给 TERM，Codex 会直接拒绝进入 TUI；
// 而 LINES/COLUMNS 与会话的真实尺寸必须一致，否则首帧布局就是错的。
// 允许用户配置它们，只会制造一类"配置看起来生效了、但 TUI 起不来"
// 的难查问题。用户要加自己的变量，加别的键即可。
func TestBuildEnvTerminalVarsAreNotOverridable(t *testing.T) {
	c := DefaultConfig()
	c.Env = map[string]string{
		"TERM":      "screen-256color",
		"COLORTERM": "24bit",
		"LINES":     "999",
		"COLUMNS":   "999",
		"CODEGATE":  "0",
	}

	env := BuildEnv(c, 120, 40)

	for k, want := range map[string]string{
		"TERM":      "xterm-256color",
		"COLORTERM": "truecolor",
		"LINES":     "40",
		"COLUMNS":   "120",
		"CODEGATE":  "1",
	} {
		if got := envValue(env, k); got != want {
			t.Errorf("%s = %q，期望被强制为 %q", k, got, want)
		}
	}
}

// TestBuildEnvNoDuplicateKeys 覆盖 Windows 的大小写不敏感语义。
//
// `Path` 和 `PATH` 是同一个变量。如果实现直接 append，
// 环境块里会出现两条同名记录，而 Windows 取哪一条是未定义的 ——
// 表现为"有时候 Path 生效有时候不生效"这种极难复现的问题。
func TestBuildEnvNoDuplicateKeys(t *testing.T) {
	env := BuildEnv(DefaultConfig(), 80, 24)

	seen := make(map[string]int, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[strings.ToUpper(k)]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("环境变量 %s 出现了 %d 次（大小写不敏感地重复）", k, n)
		}
	}
}

func TestDescribePlatform(t *testing.T) {
	platform, arch := DescribePlatform()
	if platform == "" || arch == "" {
		t.Errorf("平台标识为空: %q/%q", platform, arch)
	}
}
