//go:build windows

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/jojo/codegate/internal/terminal"
)

// 本文件是 P2 里价值最高的一项验证：**Ctrl+C 在 ConPTY 上到底怎么实现**。
//
// # 起因
//
// Agent 的 Signal(SignalInterrupt) 在 Windows 上实现为「往 PTY 写 0x03」——
// 因为 ConPTY 没有公开的注入 Ctrl+C 的接口。R1 的结论
// （PHASE0-R1-CONPTY-FINDINGS §7）是「0x03 不产生 CTRL_C_EVENT，
// 子进程收到的是字符（ORD=3）」，并据此认为这个实现是可接受的。
//
// 但用 ttyprobe 实测发现对不上：探针**根本没读到 0x03**，它的 stdin
// 直接 EOF 了。差异的根源是**控制台模式** —— 探针没设 raw mode。
//
// 于是有了本文件的三个测试：
//   - TestInterruptAgainstCmd : 用真实 shell 证伪「写 0x03」这条路
//   - TestCtrlEventProbe      : 验证标准 GenerateConsoleCtrlEvent 是否可行
//   - TestConptyDllExports    : 探测 Windows Terminal 用的私有接口是否存在
//
// ★ 三条路全部实测过了，结论写在各测试的注释里。这是 P2 作为
// 「风险关卡」的核心产出 —— 它确实卡住了一个真问题。

// ---------------------------------------------------------------------------
// 公共夹具：起一个交互式 cmd.exe
// ---------------------------------------------------------------------------

type cmdSession struct {
	term terminal.Terminal

	mu       sync.Mutex
	buf      bytes.Buffer
	readDone chan struct{}
}

func startCmd(t *testing.T) *cmdSession {
	t.Helper()

	if err := terminal.Available(); err != nil {
		t.Skipf("ConPTY 不可用: %v", err)
	}

	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}

	term := terminal.New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	if err := term.Start(ctx, terminal.StartConfig{
		Command: comspec,
		Dir:     t.TempDir(),
		Env:     probeEnv(),
		Cols:    100,
		Rows:    30,
	}); err != nil {
		t.Fatalf("启动 cmd.exe 失败: %v", err)
	}

	c := &cmdSession{term: term, readDone: make(chan struct{})}
	go func() {
		defer close(c.readDone)
		b := make([]byte, 8192)
		for {
			n, err := term.Read(b)
			if n > 0 {
				c.mu.Lock()
				c.buf.Write(b[:n])
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() { _ = term.Close() })
	return c
}

func (c *cmdSession) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *cmdSession) waitFor(substr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(c.output(), substr) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (c *cmdSession) send(s string) {
	if _, err := c.term.Write([]byte(s)); err != nil {
		// 只记录，不 fatal —— 本文件里的测试大多是在观察异常路径。
		_ = err
	}
}

// countReplies 数 ping 回复的行数，作为「进程是否还在输出」的指标。
func (c *cmdSession) countReplies() int {
	return strings.Count(c.output(), "127.0.0.1")
}

// ---------------------------------------------------------------------------
// 路径 1：写 0x03 —— 已证伪
// ---------------------------------------------------------------------------

// TestInterruptAgainstCmd 用真实 cmd.exe 判定 0x03 的实际语义。
//
// ★ 实测结论（2026-09-28，Windows 10.0.26100）：
//
//	写 0x03 之后 ——
//	  · 当前命令【没有】被中断（ping 继续输出）
//	  · shell 【不再接受】后续输入（echo 的标记串再也没出现）
//	  · 进程还活着，PTY 输出链路完全正常
//
//	对照实验证明这不是「输入通道坏了」：在写 0x03 之前，
//	同样方式发送的 echo 命令能正常执行。
//
// 也就是说：**写 0x03 既不能中断命令，还会把 stdin 弄坏。**
// 这条路必须换掉 —— 见另外两个测试。
//
// 这个测试现在作为**回归基线**存在：断言的是「当前实现的错误行为」。
// 等 Signal 换成正确实现后，本测试要改成断言正确行为
// （ping 停止 + shell 继续可用）。
func TestInterruptAgainstCmd(t *testing.T) {
	c := startCmd(t)

	// 对照组：确认连续输入本身是通的。
	c.send("echo FIRST_MARKER\r")
	if !c.waitFor("FIRST_MARKER", 10*time.Second) {
		t.Fatalf("对照组失败：连普通命令都发不进去，实际:\n%q", c.output())
	}
	t.Logf("对照组通过：普通命令能正常送达并执行")

	// 被中断对象：一个会持续输出的命令。
	c.send("ping -n 30 127.0.0.1\r")
	if !c.waitFor("127.0.0.1", 15*time.Second) {
		t.Fatalf("ping 没有开始输出，实际:\n%q", c.output())
	}
	time.Sleep(2 * time.Second)
	before := c.countReplies()

	n, err := c.term.Write([]byte{0x03})
	t.Logf("Write(0x03) -> n=%d err=%v", n, err)

	time.Sleep(3 * time.Second)
	mid := c.countReplies()

	c.send("echo AFTER_CTRL_C\r")
	alive := c.waitFor("AFTER_CTRL_C", 8*time.Second)

	t.Logf("================ 观察结论 ================")
	t.Logf("ping 输出增量            : %d（>0 表示未被中断）", mid-before)
	t.Logf("shell 是否仍接受新命令   : %v", alive)

	// 把当前的错误行为钉成回归基线。
	if mid-before == 0 {
		t.Errorf("ping 被中断了 —— 行为与 2026-09-28 的实测不同，" +
			"说明 ConPTY 的 0x03 语义变了，Signal 的实现要重新评估")
	}
	if alive {
		t.Errorf("shell 仍能接受命令 —— 行为与 2026-09-28 的实测不同，" +
			"0x03 也许已经变成可用方案，请重新验证")
	}
}

// ---------------------------------------------------------------------------
// 路径 2：GenerateConsoleCtrlEvent —— 已证伪
// ---------------------------------------------------------------------------

var (
	procFreeConsole              = kernel32.NewProc("FreeConsole")
	procAttachConsole            = kernel32.NewProc("AttachConsole")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
	procSetConsoleCtrlHandler    = kernel32.NewProc("SetConsoleCtrlHandler")
	procGetConsoleProcessList    = kernel32.NewProc("GetConsoleProcessList")
)

const ctrlCEvent = 0 // CTRL_C_EVENT

// TestCtrlEventProbe 验证标准 GenerateConsoleCtrlEvent 能否中断 ConPTY 子进程。
//
// ★ 实测结论：**不能**。而且这次能确定不是「API 用错了」——
//
//	3b) GetConsoleProcessList 返回 [测试进程, ping, cmd.exe]，
//	    说明 AttachConsole 真的生效了，三个进程确实共享同一个伪控制台，
//	    满足 GenerateConsoleCtrlEvent 文档要求的全部前置条件；
//	4)  GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) 返回非 0（成功）；
//	但  ping 依旧继续输出，shell 依旧不接受输入。
//
//	结论：系统自带的 ConPTY（kernel32 的 CreatePseudoConsole）**不响应**
//	GenerateConsoleCtrlEvent。这不是调用姿势问题，是实现缺失。
//
// # 为什么需要 AttachConsole
//
// GenerateConsoleCtrlEvent 只能给「调用者当前所附加的控制台」里的进程
// 发信号。ConPTY 给每个会话建了独立的伪控制台，Agent 默认不在里面。
//
// # 这一步的副作用（若将来启用，必须写进设计）
//
//	FreeConsole() 会让 **Agent 自己**脱离当前控制台；
//	AttachConsole 是**进程级**的全局状态，并发中断两个会话会互相踩，
//	必须整体串行化。所以即使它能用，代价也不小。
func TestCtrlEventProbe(t *testing.T) {
	c := startCmd(t)

	c.send("ping -n 30 127.0.0.1\r")
	if !c.waitFor("127.0.0.1", 15*time.Second) {
		t.Fatalf("ping 没有开始输出，实际:\n%q", c.output())
	}
	time.Sleep(2 * time.Second)
	before := c.countReplies()

	pid := uint32(c.term.PID())
	t.Logf("子进程 PID = %d", pid)

	// 步骤 1：先让自己忽略 Ctrl+C。
	// 必须在 FreeConsole 之前做 —— 一旦 AttachConsole 成功，
	// 我们就成了目标控制台的成员，GenerateConsoleCtrlEvent(…, 0)
	// 会连自己一起打。没有这一步，测试进程会被自己发的 Ctrl+C 杀掉。
	r1, _, e1 := procSetConsoleCtrlHandler.Call(0, 1) // NULL, TRUE = 忽略
	t.Logf("1) SetConsoleCtrlHandler(NULL, TRUE) -> r1=%d err=%v", r1, e1)

	// 步骤 2：脱离当前控制台。
	r2, _, e2 := procFreeConsole.Call()
	t.Logf("2) FreeConsole() -> r1=%d err=%v", r2, e2)

	// 步骤 3：附加到子进程的控制台。
	r3, _, e3 := procAttachConsole.Call(uintptr(pid))
	t.Logf("3) AttachConsole(%d) -> r1=%d err=%v", pid, r3, e3)

	// 步骤 3b：验证「附加成功」是不是真的。
	//
	// AttachConsole 返回非 0 只说明调用没报错，不说明我们真的进了那个控制台。
	// GetConsoleProcessList 给出当前控制台里**实际**的进程列表 ——
	// 这是区分「API 用错了」和「ConPTY 不支持」的关键一步。
	var pids [64]uint32
	np, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	n := int(np)
	if n > len(pids) {
		n = len(pids)
	}
	t.Logf("3b) GetConsoleProcessList -> %d 个进程: %v (目标 PID=%d)", np, pids[:n], pid)

	// 步骤 4：发 Ctrl+C 给该控制台里的所有进程（进程组 0 = 全部）。
	r4, _, e4 := procGenerateConsoleCtrlEvent.Call(uintptr(ctrlCEvent), 0)
	t.Logf("4) GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) -> r1=%d err=%v", r4, e4)

	// 步骤 5：脱身。
	r5, _, e5 := procFreeConsole.Call()
	t.Logf("5) FreeConsole() -> r1=%d err=%v", r5, e5)

	time.Sleep(3 * time.Second)
	mid := c.countReplies()

	c.send("echo AFTER_CTRL_EVENT\r")
	alive := c.waitFor("AFTER_CTRL_EVENT", 8*time.Second)

	// 恢复自己的 Ctrl+C 处理。
	procSetConsoleCtrlHandler.Call(0, 0)

	t.Logf("================ 探索结论 ================")
	t.Logf("ping 输出增量            : %d（0 = 中断成功）", mid-before)
	t.Logf("shell 是否仍接受新命令   : %v", alive)
	t.Logf("------------------------------------------")
	t.Logf("最终输出:\n%q", c.output())

	// 钉住「标准 API 无效」这个事实。
	if mid-before == 0 {
		t.Errorf("GenerateConsoleCtrlEvent 生效了 —— 与 2026-09-28 的实测不同，" +
			"说明系统 ConPTY 补上了这个能力，Signal 可以用它实现")
	}
}

// ---------------------------------------------------------------------------
// 路径 3：conpty.dll 私有接口 —— 本机不存在
// ---------------------------------------------------------------------------

// TestConptyDllExports 探测 conpty.dll 是否提供私有的信号接口。
//
// ★ 实测结论：**本机没有 conpty.dll**（System32 与 Program Files 全盘搜索均无，
// 也没装 Windows Terminal）。所以这条路在开发机上无法验证。
//
// 背景：Windows Terminal 自带一份 conpty.dll，导出
// ConptyGenerateConsoleCtrlEvent —— 它直接给伪控制台里的进程组发信号，
// 不走「附加控制台」那套机制。这被认为是 Windows 上可靠中断 ConPTY
// 子进程的正解。
//
// 对 CodeGate 的意义：如果最终要走这条路，就意味着 Agent 需要**随包分发**
// 一份 conpty.dll，而不是只用系统 API。这是一个不小的交付物变化，
// 需要单独评估（许可、体积、版本兼容）。本测试留在这里作为
// 「为什么我们要考虑这个」的证据。
func TestConptyDllExports(t *testing.T) {
	dll := syscall.NewLazyDLL("conpty.dll")
	anyFound := false
	for _, name := range []string{
		"ConptyGenerateConsoleCtrlEvent",
		"ConptyCreatePseudoConsole",
		"ConptyResizePseudoConsole",
		"ConptyClosePseudoConsole",
	} {
		if err := dll.NewProc(name).Find(); err != nil {
			t.Logf("conpty.dll!%-32s 不存在", name)
		} else {
			t.Logf("conpty.dll!%-32s ✅ 可用", name)
			anyFound = true
		}
	}
	if !anyFound {
		t.Logf("本机没有可用的 conpty.dll —— 私有信号接口这条路在开发机上无法验证")
	}
}
