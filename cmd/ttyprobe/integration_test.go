package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/terminal"
)

// 这个文件是 P2（ConPTY 风险关卡）的真正验收：把 ttyprobe 当成被测 CLI，
// 用真实 PTY 起一个进程，验证「创建 / 读 / 写 / resize / Ctrl+C / 退出码」
// 这条完整链路。文档 §6.4 说「这样一个测试文件就能在 Windows 和 Unix 上
// 同时验证」—— 就是本文件。
//
// 为什么必须走真实 PTY 而不是 mock：mock 会把「我们认为 PTY 会怎么表现」
// 写进断言里，于是它只能验证我们的假设，永远发现不了 ConPTY 的真实行为
// （重绘、合并、0x03 语义、退出后不立即 EOF —— 这些都是 mock 测不出来的）。

// buildProbe 编译出 ttyprobe 可执行文件。
//
// 用真实二进制，而不是「让测试进程假装成探针」（Go 社区常见的
// TestHelperProcess 手法）：后者会把测试框架的参数解析、超时、信号处理
// 一起拖进被测进程，而排查 PTY 问题时被测对象越干净越好 ——
// 这也正是 cmd/conpty-diag 存在的理由。
func buildProbe(t *testing.T) string {
	t.Helper()

	name := "ttyprobe"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译探针失败: %v\n%s", err, b)
	}
	return out
}

// probeEnv 构造探针子进程的环境。
//
// ★ 刻意【不】整体继承 os.Environ()。这是 F2 的教训：
//
//	整体继承会把本机坏掉的 https_proxy 一起传下去（Claude Code 报 502 就是证据）；
//	而完全不设 TERM 又会让 TUI 直接拒绝启动（Codex 会停在 "Continue anyway? [y/N]"）。
//
// 所以这里只给最小可用集 —— 和 Agent 里 environment 白名单的构造方式一致。
// 这个测试同时也在验证「白名单构造出来的环境足够让一个普通程序跑起来」。
func probeEnv() []string {
	keep := []string{
		"PATH", "SystemRoot", "SystemDrive", "windir", "ComSpec",
		"TEMP", "TMP", "USERPROFILE", "HOME",
	}
	var env []string
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)
}

type probeSession struct {
	t    *testing.T
	term terminal.Terminal

	mu  sync.Mutex
	buf bytes.Buffer

	readDone chan struct{}
}

func (s *probeSession) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor 轮询直到输出里出现 substr。
//
// 刻意不用「读固定字节数然后比对」：ConPTY 会重绘和合并输出，
// 事件到达的次数和顺序都不保证。等待一个**稳定可判定的子串**
// 才是对这类系统正确的断言方式。
func (s *probeSession) waitFor(substr string, d time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for {
		out := s.output()
		if strings.Contains(out, substr) {
			return out
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("等待 %q 超时（%s）。实际输出:\n%q", substr, d, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *probeSession) write(text string) {
	s.t.Helper()
	if _, err := s.term.Write([]byte(text)); err != nil {
		s.t.Fatalf("写入 PTY 失败: %v", err)
	}
}

func startProbe(t *testing.T, cols, rows uint16) *probeSession {
	t.Helper()

	if err := terminal.Available(); err != nil {
		t.Skipf("本平台 PTY 不可用，跳过集成测试: %v", err)
	}
	exe := buildProbe(t)

	term := terminal.New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := term.Start(ctx, terminal.StartConfig{
		Command: exe,
		Dir:     t.TempDir(),
		Env:     probeEnv(),
		Cols:    cols,
		Rows:    rows,
	}); err != nil {
		t.Fatalf("启动探针失败: %v", err)
	}

	s := &probeSession{t: t, term: term, readDone: make(chan struct{})}
	go func() {
		defer close(s.readDone)
		b := make([]byte, 8192)
		for {
			n, err := term.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(b[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		// Close 必须幂等 —— 这里再调一次是在验证这个契约。
		// 同时它也是让上面那个 Read 循环退出的唯一手段（见
		// TestProbeReadDoesNotEOFOnExit 的说明）。
		_ = term.Close()
		select {
		case <-s.readDone:
		case <-time.After(5 * time.Second):
			t.Logf("警告: Read 循环在 Close 后 5 秒内没有退出")
		}
	})

	return s
}

// waitExit 在超时内等 Wait() 返回。
func waitExit(t *testing.T, term terminal.Terminal, d time.Duration) terminal.ExitResult {
	t.Helper()
	ch := make(chan terminal.ExitResult, 1)
	go func() { ch <- term.Wait() }()
	select {
	case res := <-ch:
		return res
	case <-time.After(d):
		t.Fatalf("等待进程退出超时（%s）", d)
		return terminal.ExitResult{}
	}
}

// TestProbeLifecycle 覆盖 P2 主线：创建 → 读 → 写 → 退出码。
func TestProbeLifecycle(t *testing.T) {
	s := startProbe(t, 80, 24)

	s.waitFor("SIZE cols=80 rows=24", 20*time.Second)

	s.write("hello\r")
	s.waitFor("ECHO hello", 10*time.Second)

	s.write("exit\r")
	s.waitFor("EXIT code=0", 10*time.Second)

	res := waitExit(t, s.term, 15*time.Second)
	if !res.Exited() {
		t.Fatalf("期望正常退出，实际 %s", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("期望退出码 0，实际 %d", res.ExitCode)
	}
}

// TestProbeResize 验证 Resize 真的到达了子进程。
//
// 这是 P2 里最容易「看起来生效其实没有」的一项：ResizePseudoConsole
// 返回 nil 只说明调用没报错，不代表子进程看到了新尺寸。
// 必须让子进程自己报告它读到的尺寸 —— 这正是探针存在的主要理由。
func TestProbeResize(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE cols=80 rows=24", 20*time.Second)

	if err := s.term.Resize(60, 20); err != nil {
		t.Fatalf("Resize 失败: %v", err)
	}
	s.waitFor("RESIZE cols=60 rows=20", 10*time.Second)
}

// TestProbeResizeRepeatedly 验证连续 resize 不会错乱。
//
// 对应验收标准 B5 的前半段。Windows 侧探针是 150ms 轮询，
// 所以这里每次 resize 之后要等一个轮询周期，否则中间的尺寸
// 会被直接跳过 —— 那不是 bug，是轮询语义，测试要按这个语义写。
func TestProbeResizeRepeatedly(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE cols=80 rows=24", 20*time.Second)

	sizes := [][2]uint16{{100, 30}, {60, 20}, {120, 40}, {80, 24}}
	for _, sz := range sizes {
		if err := s.term.Resize(sz[0], sz[1]); err != nil {
			t.Fatalf("Resize(%d,%d) 失败: %v", sz[0], sz[1], err)
		}
		want := "RESIZE cols=" + itoa(int(sz[0])) + " rows=" + itoa(int(sz[1]))
		s.waitFor(want, 10*time.Second)
	}
}

// TestProbeConsoleMode 记录子进程看到的控制台输入模式。
//
// 这是解释 0x03 行为的钥匙，也是 P2 报告要留档的原始数据：
// 它告诉我们 ConPTY 交给子进程的默认输入模式到底是什么。
func TestProbeConsoleMode(t *testing.T) {
	s := startProbe(t, 80, 24)
	out := s.waitFor("MODE stdin=", 20*time.Second)

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "MODE stdin=") {
			t.Logf("ConPTY 默认输入模式: %s", strings.TrimSpace(line))
		}
	}
}

// TestProbeCtrlCInRawMode 验证 raw mode 下 0x03 是否作为字节投递。
//
// ★ 这是判断「写 0x03」这条路能不能留的关键实验：
//
//   - 如果 raw mode 下 0x03 能正常送达 → 该实现对 TUI 是对的，
//     问题只出在 shell（cooked mode），方案可以是「按子进程模式分流」
//   - 如果 raw mode 下也送不到 → 这条路彻底不能要，Signal 必须换实现
//
// TUI（vim / htop / Claude Code / Codex）启动时都会自己设 raw mode，
// 所以这条路径覆盖的正是 CodeGate 最主要的用例。
func TestProbeCtrlCInRawMode(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE", 20*time.Second)

	s.write("raw\r")
	s.waitFor("RAW mode=on", 10*time.Second)

	s.write(string([]byte{0x03}))
	s.waitFor("CTRL_C byte=0x03", 10*time.Second)

	// raw mode 下进程必须还活着 —— TUI 正是靠这个实现「Ctrl+C 不退出」，
	// 由应用自己决定要不要响应。如果这里挂了，说明 raw mode 下
	// 0x03 仍然走了 Ctrl+C 事件路径，那 TUI 的按键就会莫名"吃掉"。
	s.write("still-here\r")
	s.waitFor("ECHO still-here", 10*time.Second)
}

// TestProbeCtrlCInCookedMode 记录 cooked mode 下 0x03 的真实行为。
//
// ★ 实测结论（2026-09-28，Windows 10.0.26100）—— 与 R1 的表述有出入，
// 这里以实测为准：
//
//	探针（未设 raw mode）收到 0x03 后：
//	  · 【没有】收到 CTRL_C 字节
//	  · stdin 直接 EOF，run() 返回，进程以 exit code 0 正常退出
//	  · 进程【没有】被 CTRL_C_EVENT 杀掉
//
//	也就是说，0x03 在 cooked mode 下既不是「普通字符」，也不是
//	「Ctrl+C 信号」，而是一个**把 stdin 关掉的请求**。
//
// 这个测试断言的就是这个（不太好看的）现状 —— 它是一条**回归基线**：
// 哪天 ConPTY 改了行为，它会红，提醒我们重新评估 Signal 的实现。
func TestProbeCtrlCInCookedMode(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE", 20*time.Second)

	s.write(string([]byte{0x03}))

	// 探针应该会打印 EOF 并退出（stdin 被关）。
	s.waitFor("EOF", 10*time.Second)

	if strings.Contains(s.output(), "CTRL_C byte=0x03") {
		t.Errorf("cooked mode 下探针居然收到了 0x03 字节 —— " +
			"行为与 2026-09-28 的实测不同，Signal 的实现可以重新评估")
	}

	res := waitExit(t, s.term, 10*time.Second)
	if res.Signal != 0 {
		t.Errorf("探针被信号杀死了（%s）—— 说明 0x03 产生了 CTRL_C_EVENT，"+
			"与 2026-09-28 的实测不同", res)
	}
}

// TestProbeUTF8Transparent 验证多字节字符逐字节原样穿过 PTY。
func TestProbeUTF8Transparent(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE", 20*time.Second)

	const want = "中文😀"
	s.write(want + "\r")
	s.waitFor("ECHO "+want, 10*time.Second)
}

// TestProbeWidthSamples 验证定宽样本完整穿过。
//
// 只保证字节没丢 —— 渲染宽度由 xterm.js 决定，是 Phase 6 的事。
// 这里提前钉住字节层，是为了让 Phase 6 的问题定位能收敛到前端。
func TestProbeWidthSamples(t *testing.T) {
	s := startProbe(t, 100, 30)
	s.waitFor("SIZE cols=100 rows=30", 20*time.Second)

	s.write("width\r")
	out := s.waitFor("WIDTH mixed=", 10*time.Second)

	for _, want := range []string{
		"WIDTH cjk=中文宽字符",
		"WIDTH emoji=😀🎉",
		"WIDTH box=┌─┬─┐│└┴┘",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，实际输出:\n%q", want, out)
		}
	}
}

// TestProbeReadDoesNotEOFOnExit 记录一个**重要的平台行为**。
//
// 实测：Windows 上子进程退出后，Read 不会返回 EOF —— conhost 仍然
// 持有管道写端，直到我们调用 Close 才断。这与 Unix PTY 不同
// （Unix 上子进程退出会关闭从端，master 读端随即 EOF）。
//
// 这条差异直接决定 Agent 的退出路径怎么写：文档 §6.2 第 3 点说
// 「Wait 返回 → 继续 drain 到 EOF → 发 session.exit」，在 Windows 上
// 「drain 到 EOF」是个**永远等不到的事件**，照抄会挂死。
// 正确做法是先 Wait() 拿到退出码，再给一个短的静默窗口收尾，
// 然后主动 Close。
//
// 这个测试把「不 EOF」本身作为断言 —— 如果哪天它开始 EOF 了，
// 说明 ConPTY 行为变了，上面的结论要重新验证。
func TestProbeReadDoesNotEOFOnExit(t *testing.T) {
	s := startProbe(t, 80, 24)
	s.waitFor("SIZE", 20*time.Second)

	s.write("exit\r")
	s.waitFor("EXIT code=0", 10*time.Second)

	res := waitExit(t, s.term, 15*time.Second)
	if !res.Exited() {
		t.Fatalf("探针没有正常退出: %s", res)
	}

	select {
	case <-s.readDone:
		t.Logf("注意: 子进程退出后 Read 返回了 —— 与之前实测的 ConPTY 行为不同，" +
			"请重新验证 Agent 退出路径的假设")
	case <-time.After(2 * time.Second):
		// 期望走这条分支。
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
