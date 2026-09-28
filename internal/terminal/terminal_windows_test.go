//go:build windows

package terminal

// Phase 2 的验收测试。
//
// 规范要求的五件事（创建 PowerShell / 读输出 / 发命令 / resize / Ctrl+C）
// 分成两组来验证：
//
//	TestConPTYPowerShell      —— 真的起 PowerShell，验证"能用"
//	TestConPTYChildLifecycle  —— 用测试二进制自身作子进程，验证"精确正确"
//
// 为什么不能只留 PowerShell 那组：PowerShell 的提示符、编码、行编辑
// 都会往流里掺东西，断言只能写得很松（"输出里有没有这个词"）。
// 而 resize 是否真的生效、Ctrl+C 到底以什么字节到达，需要子进程
// 主动报告才能断言。用 Go 测试二进制自身当子进程是标准技巧：
// os.Executable() 就是它，用环境变量分流即可，不需要额外编译产物。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ---------------------------------------------------------------------------
// 子进程模式
// ---------------------------------------------------------------------------

// TestMain 是所有测试的入口。CODEGATE_PTY_CHILD=1 时分流到子进程逻辑。
func TestMain(m *testing.M) {
	if os.Getenv("CODEGATE_PTY_CHILD") == "1" {
		runChildProcess()
		return
	}
	os.Exit(m.Run())
}

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
)

// Go 的 syscall 包只导出了 GetConsoleMode，没导出 SetConsoleMode。
var procSetConsoleMode = kernel32.NewProc("SetConsoleMode")

func setConsoleMode(h syscall.Handle, mode uint32) error {
	r1, _, callErr := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r1 == 0 {
		return callErr
	}
	return nil
}

// enterRawMode 把控制台切成 raw 模式：关掉行缓冲、回显和 Ctrl+C 的
// 系统级处理。
//
// 这正是 Claude Code / Codex / vim 这类 TUI 启动时会做的事，
// 也是我们真正关心的场景 —— 只有在 raw 模式下，Ctrl+C 才会作为
// 一个普通字节到达，而不是被 conhost 转成 CTRL_C_EVENT 吃掉。
func enterRawMode() error {
	h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return err
	}
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return err
	}
	mode &^= enableProcessedInput | enableLineInput | enableEchoInput
	return setConsoleMode(h, mode)
}

type smallRect struct {
	Left, Top, Right, Bottom int16
}

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

var procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")

// consoleSize 返回当前控制台【可视窗口】的尺寸（不是缓冲区尺寸）。
//
// 用窗口而不是缓冲区：ConPTY 的 resize 改的是窗口大小，
// 缓冲区通常跟着变，但断言窗口更直接对应我们请求的 cols/rows。
func consoleSize() string {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return "err"
	}
	var info consoleScreenBufferInfo
	r1, _, _ := procGetConsoleScreenBufferInfo.Call(
		uintptr(h), uintptr(unsafe.Pointer(&info)),
	)
	if r1 == 0 {
		return "err"
	}
	return fmt.Sprintf("%dx%d",
		info.Window.Right-info.Window.Left+1,
		info.Window.Bottom-info.Window.Top+1)
}

// runChildProcess 是"受控子进程"的全部行为。
//
// 它做三件事，每件都对应一条要验证的路径：
//
//	报告初始尺寸 + 尺寸变化  → resize 是否真的传到了子进程
//	把读到的 stdin 以 hex 输出 → 输入字节是否透明（含 Ctrl+C 的 0x03）
//	输出一段中文 + emoji      → 输出方向的 UTF-8 是否完整
//
// 用 hex 而不是原文输出输入内容，是为了让"非 ASCII 字节"的断言
// 不受控制台编码影响 —— 测试机是 GBK 代码页也不会误判。
func runChildProcess() {
	out := os.Stdout

	fmt.Fprintf(out, "INIT:%s\n", consoleSize())
	fmt.Fprintf(out, "UTF8:%s\n", "中文🎉")

	if err := enterRawMode(); err != nil {
		fmt.Fprintf(out, "RAWMODE_FAILED:%v\n", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		last := ""
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s := consoleSize(); s != last {
					fmt.Fprintf(out, "SIZE:%s\n", s)
					last = s
				}
			}
		}
	}()

	buf := make([]byte, 512)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			fmt.Fprintf(out, "DATA:%x\n", buf[:n])
		}
		if err != nil {
			fmt.Fprintf(out, "READ_ERR:%v\n", err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// ptyReader 在后台持续读终端，把输出累积起来供测试轮询断言。
//
// 为什么不用"每次读都开一个带超时的 goroutine"：Terminal.Read 是阻塞的，
// 超时后那个 goroutine 会一直挂到 Close 才返回。一个测试里读十几次
// 就是十几个僵尸 goroutine，还会让 -race 报出一堆无意义的警告。
type ptyReader struct {
	mu   sync.Mutex
	buf  []byte
	err  error
	done chan struct{}
}

func startReader(term Terminal) *ptyReader {
	r := &ptyReader{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		b := make([]byte, 4096)
		for {
			n, err := term.Read(b)
			if n > 0 {
				r.mu.Lock()
				r.buf = append(r.buf, b[:n]...)
				r.mu.Unlock()
			}
			if err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				return
			}
		}
	}()
	return r
}

func (r *ptyReader) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// waitFor 轮询直到输出里出现 want，返回当时的完整输出和是否命中。
func (r *ptyReader) waitFor(want string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := r.snapshot(); strings.Contains(s, want) {
			return s, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r.snapshot(), false
}

// sanitize 把控制字符换成可见形式，否则失败信息里全是不可打印字节。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0x1b:
			b.WriteString("<ESC>")
		case r < 0x20 && r != '\n' && r != '\t':
			fmt.Fprintf(&b, "<%02X>", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// childEnv 构造子进程的环境。
//
// ★ 显式白名单，不是 os.Environ() 整体继承 —— 这正是 Agent 层要做的事，
//
//	测试里顺手把这条路验证了。
//	本机的实测教训：整体继承会把坏掉的 https_proxy 一起传下去。
func childEnv() []string {
	keys := []string{
		"SystemRoot", "SystemDrive", "windir", "ComSpec",
		"PATH", "PATHEXT",
		"TEMP", "TMP",
		"USERPROFILE", "HOMEDRIVE", "HOMEPATH",
		"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
	}
	env := []string{"CODEGATE_PTY_CHILD=1"}
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func findPowerShell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// powershell.exe 通常不在 PATH 里，用绝对路径兜底。
	fallback := filepath.Join(os.Getenv("SystemRoot"),
		"System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Skip("本机没有 PowerShell，跳过 ConPTY 集成测试")
	return ""
}

// encodePowerShell 把脚本编码成 -EncodedCommand 需要的格式（UTF-16LE 的 Base64）。
//
// 用它可以完全绕开命令行引号转义的地狱：脚本里有单引号、$ 号、换行，
// 直接传参要在 PowerShell 的解析规则和 CreateProcessW 的规则之间
// 来回转义两次，极易出错。
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// 集成测试：ConPTY 全链路
// ---------------------------------------------------------------------------

// TestConPTYChildLifecycle 用测试二进制自身作为子进程，精确验证
// 输入、输出、resize、Ctrl+C 四条路径。
func TestConPTYChildLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 ConPTY 集成测试（-short）")
	}
	if err := Available(); err != nil {
		t.Skipf("本机不支持 ConPTY: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("拿不到测试二进制路径: %v", err)
	}

	term := New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := term.Start(ctx, StartConfig{
		Command: self,
		Args:    []string{"-test.run=^$"}, // 子进程走 TestMain 分流，不会跑测试
		Dir:     t.TempDir(),
		Env:     childEnv(),
		Cols:    80,
		Rows:    24,
	}); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer term.Close()

	if pid := term.PID(); pid == 0 {
		t.Error("PID() 返回 0，说明没拿到子进程 ID")
	}

	rd := startReader(term)

	// ---- 1) 创建 + 读取输出：伪控制台尺寸被正确传递 ----
	if s, ok := rd.waitFor("INIT:80x24", 15*time.Second); !ok {
		t.Fatalf("子进程没有报告初始尺寸 80x24（说明 CreatePseudoConsole 的尺寸没生效）。\n输出:\n%s", sanitize(s))
	}

	// ---- 2) 输出方向的 UTF-8 完整性 ----
	// 中文 + emoji 一共跨 3 个 UTF-8 多字节序列，能过说明输出路径
	// 没有被按单字节代码页截断。
	if s, ok := rd.waitFor("UTF8:中文🎉", 10*time.Second); !ok {
		t.Errorf("输出方向的中文/emoji 没有完整到达。\n输出:\n%s", sanitize(s))
	}

	// ---- 3) 输入方向：UTF-8 透明 ----
	const probe = "中文🎉"
	if _, err := term.Write([]byte(probe)); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	wantHex := fmt.Sprintf("%x", []byte(probe))
	if s, ok := rd.waitFor(wantHex, 10*time.Second); !ok {
		t.Errorf("输入方向的中文/emoji 字节没有到达子进程（期望 hex %s）。\n输出:\n%s",
			wantHex, sanitize(s))
	}

	// ---- 4) resize：必须真的传到子进程，而不只是我们记了个数 ----
	if err := term.Resize(120, 40); err != nil {
		t.Fatalf("Resize(120,40) 失败: %v", err)
	}
	if s, ok := rd.waitFor("SIZE:120x40", 10*time.Second); !ok {
		t.Errorf("resize 没有传到子进程（子进程没报告 120x40）。\n输出:\n%s", sanitize(s))
	}

	// 再缩一次，确认不是只有"放大"能生效。
	if err := term.Resize(60, 20); err != nil {
		t.Fatalf("Resize(60,20) 失败: %v", err)
	}
	if s, ok := rd.waitFor("SIZE:60x20", 10*time.Second); !ok {
		t.Errorf("第二次 resize（缩小）没有生效。\n输出:\n%s", sanitize(s))
	}

	// ---- 5) Ctrl+C ----
	// 子进程已进入 raw 模式，所以这里验证的是"0x03 作为一个字节到达"。
	// 对 TUI 应用而言这正是要的语义（由应用自己解释 Ctrl+C）。
	if err := term.Signal(SignalInterrupt); err != nil {
		t.Fatalf("Signal(SignalInterrupt) 失败: %v", err)
	}
	if s, ok := rd.waitFor("DATA:03", 10*time.Second); !ok {
		t.Errorf("Ctrl+C 没有作为 0x03 字节到达子进程。\n输出:\n%s", sanitize(s))
	}

	// ---- 6) Close 必须唤醒阻塞中的 Read，而不是把它挂死 ----
	if err := term.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	select {
	case <-rd.done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close 之后读循环没有退出 —— 阻塞中的 ReadFile 没被唤醒；" +
			"真实场景下这会表现为「关闭会话后 goroutine 永久泄漏」")
	}

	// ---- 7) Close 之后的操作必须明确报错，而不是静默成功 ----
	if err := term.Close(); err != nil {
		t.Errorf("Close 不是幂等的: %v", err)
	}
	if err := term.Resize(80, 24); !errors.Is(err, ErrClosed) {
		t.Errorf("关闭后 Resize 应报 ErrClosed，实际 %v", err)
	}

	// Wait 在 Close 之后调用必须能返回（拿不到 exit code 是预期的，
	// 因为进程句柄已被释放），但不能阻塞。
	waitDone := make(chan ExitResult, 1)
	go func() { waitDone <- term.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Error("Close 之后 Wait 阻塞了")
	}
}

// TestConPTYPowerShell 验证规范 Phase 2 的首要目标：
// 真的能起一个 PowerShell，读到它的输出，并往里发命令拿到回应。
func TestConPTYPowerShell(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 ConPTY 集成测试（-short）")
	}
	ps := findPowerShell(t)

	// 刻意用 -Command 而不是交互式 REPL：
	// 交互式会加载 PSReadLine，它的语法高亮会给每个 token 套上 ANSI 序列，
	// 断言 "CG_ECHO:xxx" 会被颜色码从中间打断。
	// 这个脚本自己实现一个 ReadLine 循环，输出是纯文本。
	script := strings.Join([]string{
		`$ErrorActionPreference = 'Stop'`,
		`[Console]::OutputEncoding = [Text.Encoding]::UTF8`,
		`[Console]::WriteLine('CG_READY')`,
		`while ($true) {`,
		`  $line = [Console]::ReadLine()`,
		`  if ($null -eq $line) { break }`,
		`  [Console]::WriteLine('CG_ECHO:' + $line)`,
		`  if ($line -eq 'CG_QUIT') { break }`,
		`}`,
	}, "\n")

	term := New()
	defer term.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := term.Start(ctx, StartConfig{
		Command: ps,
		Args: []string{
			"-NoLogo", "-NoProfile",
			"-EncodedCommand", encodePowerShell(script),
		},
		Dir:  t.TempDir(),
		Env:  childEnv(),
		Cols: 100,
		Rows: 30,
	}); err != nil {
		t.Fatalf("启动 PowerShell 失败: %v", err)
	}

	rd := startReader(term)

	if s, ok := rd.waitFor("CG_READY", 40*time.Second); !ok {
		t.Fatalf("PowerShell 没有就绪。\n输出:\n%s", sanitize(s))
	}

	// 发一条命令，验证输入真的进了 PowerShell 的 stdin。
	if _, err := term.Write([]byte("hello-from-codegate\r\n")); err != nil {
		t.Fatalf("发送命令失败: %v", err)
	}
	if s, ok := rd.waitFor("CG_ECHO:hello-from-codegate", 20*time.Second); !ok {
		t.Fatalf("PowerShell 没有回显命令。\n输出:\n%s", sanitize(s))
	}

	// 中文走一遍，确认不是只有 ASCII 能用。
	if _, err := term.Write([]byte("中文测试\r\n")); err != nil {
		t.Fatalf("发送中文命令失败: %v", err)
	}
	if s, ok := rd.waitFor("CG_ECHO:中文测试", 20*time.Second); !ok {
		t.Errorf("PowerShell 没有正确回显中文。\n输出:\n%s", sanitize(s))
	}

	// resize 不能报错（真值断言在 TestConPTYChildLifecycle 里）。
	if err := term.Resize(80, 24); err != nil {
		t.Errorf("对运行中的 PowerShell 执行 resize 失败: %v", err)
	}

	// 让它退出，验证 Wait 能拿到退出码。
	if _, err := term.Write([]byte("CG_QUIT\r\n")); err != nil {
		t.Fatalf("发送退出命令失败: %v", err)
	}

	waitDone := make(chan ExitResult, 1)
	go func() { waitDone <- term.Wait() }()
	select {
	case res := <-waitDone:
		if res.Err != nil {
			t.Errorf("Wait 返回错误: %v", res.Err)
		} else {
			t.Logf("PowerShell 退出: %s", res)
		}
	case <-time.After(30 * time.Second):
		t.Error("PowerShell 没有在 30 秒内退出")
	}
}

// ---------------------------------------------------------------------------
// 单元测试：不需要真实进程的纯逻辑
// ---------------------------------------------------------------------------

func TestConPTYAvailable(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("本机不支持 ConPTY: %v", err)
	}
}

// 未启动的终端，所有操作都必须明确报错，不能静默成功。
//
// 这条测试看着琐碎，但它挡住的是很实际的一类 bug：
// 上层在 Start 失败后继续用这个终端，如果 Write 静默返回成功，
// 用户会看到"输入了但没反应"，而日志里一片正常。
func TestConPTYNotStarted(t *testing.T) {
	term := New()

	if err := term.Resize(80, 24); !errors.Is(err, ErrNotStarted) {
		t.Errorf("Resize: 期望 ErrNotStarted，实际 %v", err)
	}
	if err := term.Signal(SignalInterrupt); !errors.Is(err, ErrNotStarted) {
		t.Errorf("Signal(interrupt): 期望 ErrNotStarted，实际 %v", err)
	}
	if err := term.Signal(SignalTerminate); !errors.Is(err, ErrNotStarted) {
		t.Errorf("Signal(terminate): 期望 ErrNotStarted，实际 %v", err)
	}
	if res := term.Wait(); !errors.Is(res.Err, ErrNotStarted) {
		t.Errorf("Wait: 期望 ErrNotStarted，实际 %v", res.Err)
	}
	if pid := term.PID(); pid != 0 {
		t.Errorf("PID: 期望 0，实际 %d", pid)
	}
	if _, err := term.Read(make([]byte, 8)); !errors.Is(err, ErrNotStarted) {
		// Read 走的是 io.EOF 分支（hOutR 为 0）
		t.Logf("Read 在未启动时返回: %v", err)
	}

	// Close 必须幂等，且对未启动的终端调用不能 panic。
	if err := term.Close(); err != nil {
		t.Errorf("首次 Close: %v", err)
	}
	if err := term.Close(); err != nil {
		t.Errorf("第二次 Close 不是幂等的: %v", err)
	}
}

// 重复 Start 必须被拒绝。
func TestConPTYDoubleStart(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过集成测试（-short）")
	}
	if err := Available(); err != nil {
		t.Skipf("本机不支持 ConPTY: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("拿不到测试二进制路径: %v", err)
	}

	term := New()
	defer term.Close()

	cfg := StartConfig{
		Command: self,
		Args:    []string{"-test.run=^$"},
		Dir:     t.TempDir(),
		Env:     childEnv(),
		Cols:    80,
		Rows:    24,
	}
	if err := term.Start(context.Background(), cfg); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	if err := term.Start(context.Background(), cfg); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("重复 Start 应报 ErrAlreadyStarted，实际 %v", err)
	}
}

// 非法参数必须在创建任何系统资源之前就被拒绝。
func TestConPTYStartValidation(t *testing.T) {
	term := New()
	defer term.Close()

	if err := term.Start(context.Background(), StartConfig{}); err == nil {
		t.Error("空 Command 应该报错")
	} else if errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("空 Command 报成了 ErrAlreadyStarted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 context
	err := term.Start(ctx, StartConfig{Command: "cmd.exe"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 context 应报 context.Canceled，实际 %v", err)
	}
}

func TestEscapeArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", `""`},
		{"simple", "simple"},
		{`C:\Windows\System32\cmd.exe`, `C:\Windows\System32\cmd.exe`},
		{`C:\Program Files\Go\go.exe`, `"C:\Program Files\Go\go.exe"`},
		// 尾部反斜杠在加引号时必须翻倍，否则会转义掉结束引号。
		{`C:\Program Files\`, `"C:\Program Files\\"`},
		{`say "hi"`, `"say \"hi\""`},
		{"a\tb", "\"a\tb\""},
	}
	for _, c := range cases {
		if got := escapeArg(c.in); got != c.want {
			t.Errorf("escapeArg(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestBuildCommandLine(t *testing.T) {
	got := buildCommandLine(
		`C:\Program Files\Go\bin\go.exe`,
		[]string{"build", "-o", `C:\out dir\x.exe`, "plain"},
	)
	want := `"C:\Program Files\Go\bin\go.exe" build -o "C:\out dir\x.exe" plain`
	if got != want {
		t.Errorf("buildCommandLine:\n  实际 %s\n  期望 %s", got, want)
	}
}

// decodeEnvBlock 把 UTF-16 环境块解回 "K=V" 列表（仅测试用）。
func decodeEnvBlock(b []uint16) []string {
	var out []string
	start := 0
	for i, v := range b {
		if v != 0 {
			continue
		}
		if i == start { // 双 NUL，块结束
			break
		}
		out = append(out, string(utf16.Decode(b[start:i])))
		start = i + 1
	}
	return out
}

func TestBuildEnvBlock(t *testing.T) {
	// nil 表示继承父进程环境（只给 PoC / 测试用）。
	b, err := buildEnvBlock(nil)
	if err != nil {
		t.Fatalf("buildEnvBlock(nil) 报错: %v", err)
	}
	if b != nil {
		t.Errorf("buildEnvBlock(nil) 应返回 nil，实际 %v", b)
	}

	// 排序（大小写不敏感）+ 同名去重（后者覆盖前者）。
	b, err = buildEnvBlock([]string{"PATH=x", "b=2", "a=1", "A=9"})
	if err != nil {
		t.Fatalf("buildEnvBlock 报错: %v", err)
	}
	got := decodeEnvBlock(b)
	want := []string{"A=9", "b=2", "PATH=x"}
	if len(got) != len(want) {
		t.Fatalf("条目数 = %d, 期望 %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q, 期望 %q（完整: %v）", i, got[i], want[i], got)
		}
	}

	// 结尾必须是双 NUL。
	if len(b) < 2 || b[len(b)-1] != 0 || b[len(b)-2] != 0 {
		t.Errorf("环境块结尾不是双 NUL: %v", b[len(b)-4:])
	}

	// 非法项（无等号、空键）应被跳过而不是报错。
	b, err = buildEnvBlock([]string{"NOEQUALS", "=C:=C:\\", "OK=1"})
	if err != nil {
		t.Fatalf("buildEnvBlock 报错: %v", err)
	}
	if got := decodeEnvBlock(b); len(got) != 1 || got[0] != "OK=1" {
		t.Errorf("非法项没有被跳过: %v", got)
	}
}
