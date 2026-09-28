//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Win32 绑定
// ---------------------------------------------------------------------------
//
// 为什么不用 syscall 包：Go 的 syscall 在 Windows 上只导出了 GetConsoleMode /
// ReadConsole / WriteConsole 三个控制台函数，**没有** GetConsoleScreenBufferInfo
// （它只在 syscall 包内部被 stdio 用来判断句柄类型）。要拿窗口尺寸只能自己绑。
//
// 这里刻意和 internal/terminal/terminal_windows.go 用同一套写法
// （NewLazyDLL + NewProc + Call），但**不共享代码**：
// 探针存在的意义之一就是独立于被测实现再确认一次行为。
// 如果它和被测代码复用同一层封装，就只能验证我们的假设，失去交叉验证的价值。

var (
	// kernel32.dll 是每个 Windows 进程初始化时已加载的基础模块，
	// 不存在 DLL 搜索路径劫持问题。
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// coord 对应 Win32 的 COORD（两个 SHORT）。
type coord struct {
	X int16
	Y int16
}

// smallRect 对应 Win32 的 SMALL_RECT。
type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

// consoleScreenBufferInfo 对应 Win32 的 CONSOLE_SCREEN_BUFFER_INFO。
//
// ★ 字段顺序和宽度必须与 Win32 结构体逐字节一致 —— 这是跨 FFI 边界的
// 结构体，多一个字段或少一个字节，Windows 就会往我们的栈上写垃圾。
// 布局：COORD(4) + COORD(4) + WORD(2) + SMALL_RECT(8) + COORD(4) = 22 字节。
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// 编译期断言：结构体大小必须是 22 字节。
// 如果哪天有人改了字段类型，这里会变成负长度的数组而编译失败 ——
// 比运行时读到垃圾数据强得多。
var _ = [1]struct{}{}[unsafe.Sizeof(consoleScreenBufferInfo{})-22]

// consoleSize 读取伪控制台的**可见窗口**尺寸。
//
// ★ 取的是 Window（可见区域）而不是 Size（缓冲区大小）。
// ConPTY 下的屏幕缓冲区通常远大于窗口，用 Size 会得到虚高的列数，
// 而 CLI 看到的、TUI 用来布局的，永远是窗口尺寸。
func consoleSize() (int, int, error) {
	var info consoleScreenBufferInfo
	r1, _, callErr := procGetConsoleScreenBufferInfo.Call(
		uintptr(syscall.Handle(os.Stdout.Fd())),
		uintptr(unsafe.Pointer(&info)),
	)
	// ★ 只在失败时看 err：Windows 的 Call 在成功时返回的 err 是
	// "The operation completed successfully." 这个**假错误**。
	if r1 == 0 {
		return 0, 0, callErr
	}
	cols := int(info.Window.Right-info.Window.Left) + 1
	rows := int(info.Window.Bottom-info.Window.Top) + 1
	return cols, rows, nil
}

// watchSize 轮询尺寸变化。
//
// Windows 没有 SIGWINCH 这类「窗口变了」的通知：ConPTY 也不会主动告诉
// 子进程 ResizePseudoConsole 被调用过。子进程能观察到的唯一迹象，
// 就是 GetConsoleScreenBufferInfo 的返回值变了。所以只能轮询。
//
// 150ms 是权衡结果：快到人工观察不出延迟，又远不至于成为 CPU 热点
// （这个调用本身极便宜，但没有理由更密）。
//
// ★ 副作用值得记一笔：这条轮询路径是 Unix 侧 SIGWINCH 的**语义替代**。
// 它意味着在 Windows 上，「窗口变化」不是事件，而是需要被发现的**状态变化**。
// 直接后果：快速连续 resize 时中间尺寸可能被整个跳过。
// 这不是 bug，是轮询语义 —— Phase 6 前端做 resize 防抖时必须知道这一点，
// 否则会把「防抖窗口小于轮询周期导致的丢帧」误判成后端 resize 链路断了。
func watchSize(stop <-chan struct{}, onChange func(cols, rows int)) {
	lastC, lastR, err := consoleSize()
	if err != nil {
		return
	}
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c, r, err := consoleSize()
			if err != nil || (c == lastC && r == lastR) {
				continue
			}
			lastC, lastR = c, r
			onChange(c, r)
		}
	}
}
