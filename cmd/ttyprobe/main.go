// Command ttyprobe 是 PTY 的测试夹具（不是产品组件，不进 Server/Agent 二进制）。
//
// 设计文档：docs/PHASE0-ARCHITECTURE.md §6.4
//
// # 为什么需要它
//
// 用「跑个 ls 看输出对不对」来测 PTY 是行不通的：ls 的输出随版本、
// TERM、locale、颜色配置而变，断言它必然脆弱，而且它测的是 ls，
// 不是 PTY。这个探针报告的是 **PTY 自身的真实属性** —— 窗口尺寸、
// 字节保真度、信号语义 —— 与任何外部命令的行为无关。
//
// # 它做什么
//
//  1. 启动时打印伪控制台尺寸          → SIZE cols=80 rows=24
//  2. 打印 stdin 的控制台输入模式      → MODE stdin=0x0003 LINE_INPUT|PROCESSED_INPUT
//  3. 尺寸变化时再打印一次            → RESIZE cols=60 rows=20
//  4. 把 stdin 原样回显（cat 行为）   → ECHO <原样内容>
//  5. 收到 0x03 时报告但不退出        → CTRL_C byte=0x03
//  6. 收到 width 命令时打印定宽样本   → WIDTH cjk=中文宽字符
//  7. 收到 raw 命令时切到 raw mode    → RAW mode=on
//  8. 收到 exit 时以 0 退出           → EXIT code=0
//
// # 输出契约
//
// 一行一个事件，格式固定为「大写标签 + 空格 + 内容」，便于测试用正则断言。
// 注意：PTY 会把 \n 转成 \r\n（ONLCR），断言时**必须容忍行尾的 \r**。
// 也不要假设一个事件一定落在一次 Read 里 —— ConPTY 会重绘和合并。
//
// # 关于 Ctrl+C 为什么不退出
//
// 实测（docs/PHASE0-R1-CONPTY-FINDINGS.md §7）：在 Windows ConPTY 上写 0x03
// 不会产生 CTRL_C_EVENT，子进程收到的就是一个普通输入字符。对 raw mode 的
// TUI 来说这正是期望行为 —— 应用自己解释它。所以探针按「字符」对待它，
// 报告后继续运行，这样才能验证「字节确实被投递过来了」。
package main

import (
	"bufio"
	"fmt"
	"os"
	"sync"
)

func main() {
	cols, rows, err := consoleSize()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttyprobe: 读取控制台尺寸失败: %v\n", err)
		os.Exit(2)
	}

	// 尺寸监视 goroutine 与主循环都会写 stdout，必须串行化，
	// 否则 RESIZE 事件可能插进 ECHO 行中间，把输出契约撕碎。
	var mu sync.Mutex
	emit := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(os.Stdout, format+"\n", args...)
	}

	emit("SIZE cols=%d rows=%d", cols, rows)

	// 报告控制台输入模式。
	// 它决定了 0x03（Ctrl+C）走哪条路径 —— 见 mode_windows.go 的说明。
	if m, err := stdinConsoleMode(); err == nil {
		emit("MODE stdin=0x%04x %s", m, describeInputMode(m))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchSize(stop, func(c, r int) {
			emit("RESIZE cols=%d rows=%d", c, r)
		})
	}()

	run(emit)

	close(stop)
	wg.Wait()
	emit("EOF")
}

// run 是主输入循环。
//
// 刻意按【字节】而不是按行读：按行读会把 0x03 吞进缓冲区里，
// 而 Ctrl+C 的投递语义恰恰是 P2 要验证的东西之一。
func run(emit func(string, ...any)) {
	br := bufio.NewReaderSize(os.Stdin, 64*1024)
	line := make([]byte, 0, 256)
	// pendingCR 用来把 \r\n 当成一个行结束符。
	// Windows 侧行尾常见 \r\n，Unix 侧是 \n，raw mode 下又可能只有 \r ——
	// 三种都要正确处理，且不能把 \r\n 算成两个空行。
	pendingCR := false

	flush := func() bool { // 返回 false 表示收到 exit，调用方应结束
		s := string(line)
		line = line[:0]
		if s == "exit" {
			emit("EXIT code=0")
			return false
		}
		if s == "width" {
			emitWidthSamples(emit)
			return true
		}
		if s == "raw" {
			// 切到 raw mode —— TUI 启动时都会这么做。
			// 它改变 0x03 的投递语义，所以必须能单独验证（见 mode_windows.go）。
			if err := setRawMode(); err != nil {
				emit("RAW error=%v", err)
			} else {
				emit("RAW mode=on")
			}
			return true
		}
		emit("ECHO %s", s)
		return true
	}

	for {
		b, err := br.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case 0x03: // Ctrl+C
			line = line[:0]
			pendingCR = false
			emit("CTRL_C byte=0x03")
		case '\r':
			pendingCR = true
			if !flush() {
				return
			}
		case '\n':
			if pendingCR {
				pendingCR = false // 已经由 \r 处理过了
				continue
			}
			if !flush() {
				return
			}
		default:
			pendingCR = false
			// 1 MB 上限：探针不是数据通道，超长行只说明有人在灌二进制。
			// 静默截断而不是无限增长，避免把测试机内存打爆。
			if len(line) < 1<<20 {
				line = append(line, b)
			}
		}
	}
}

// emitWidthSamples 打印一组定宽样本，用于验证字符宽度渲染。
//
// 这些样本挑的都是「宽度容易算错」的字符：
//   - CJK 是双宽（East Asian Width = Wide）
//   - emoji 的宽度在各终端实现里分歧最大（有按 1 算的，有按 2 算的）
//   - box drawing 是单宽，但在 CJK 字体下常被渲染成双宽
//
// 断言方式是：在固定 cols 下打印，看 xterm.js 里光标列数是否等于预期，
// 而不是「看起来对不对」。
func emitWidthSamples(emit func(string, ...any)) {
	emit("WIDTH ascii=%s", "0123456789")
	emit("WIDTH cjk=%s", "中文宽字符")
	emit("WIDTH emoji=%s", "😀🎉")
	emit("WIDTH box=%s", "┌─┬─┐│└┴┘")
	emit("WIDTH mixed=%s", "ab中c😀d")
}
