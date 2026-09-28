//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
)

var procSetConsoleMode = kernel32.NewProc("SetConsoleMode")

// stdinConsoleMode 返回 stdin 的控制台**输入**模式。
//
// ★ 为什么探针必须报告这个：
//
// 0x03（Ctrl+C）的行为**完全由输入模式决定**，不是固定语义：
//   - ENABLE_PROCESSED_INPUT (0x0001) 开着 → 0x03 由 conhost 拦截，
//     不会作为字节交给 ReadConsole/ReadFile，而是走 Ctrl+C 事件路径
//   - 关着（即 raw mode）→ 0x03 就是一个普通字节，交给应用自己解释
//
// 不报告模式，"Ctrl+C 不工作"这类问题根本没法定位 ——
// 你会同时面对两种完全不同的机制，却看不出当前是哪种。
func stdinConsoleMode() (uint32, error) {
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(os.Stdin.Fd()), &mode); err != nil {
		return 0, err
	}
	return mode, nil
}

// setRawMode 把 stdin 切成 raw mode —— 这是所有全屏 TUI（vim / htop /
// Claude Code / Codex / OpenCode）启动时都会做的事。
//
// 具体关掉三样：
//   - ENABLE_PROCESSED_INPUT : 让 Ctrl+C 不再被 conhost 拦截，而是作为
//     0x03 字节交给我们自己解释（TUI 靠这个实现"Ctrl+C 不退出"）
//   - ENABLE_LINE_INPUT      : 关掉行缓冲，按键立即到达
//   - ENABLE_ECHO_INPUT      : 关掉控制台回显，否则屏幕会重影
//
// 并打开 ENABLE_VIRTUAL_TERMINAL_INPUT：让输入以 VT 序列形式到达
// （方向键变成 \x1b[A 这种），而不是 KEY_EVENT 结构。
//
// 探针提供这个开关，是为了能把「cooked mode 下的 0x03」和
// 「raw mode 下的 0x03」两条路径分开实测 —— 它们是两套完全不同的机制。
func setRawMode() error {
	h := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return err
	}
	mode &^= 0x0001 | 0x0002 | 0x0004 // 关 PROCESSED_INPUT | LINE_INPUT | ECHO_INPUT
	mode |= 0x0200                    // 开 VIRTUAL_TERMINAL_INPUT
	r1, _, callErr := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r1 == 0 {
		return callErr
	}
	return nil
}

// describeInputMode 把输入模式标志位展开成可读字符串。
//
// 位值来自 wincon.h。这里只列有实际意义的那些 ——
// 探针的输出是给人看的诊断信息，不是完整的 API 镜像。
func describeInputMode(mode uint32) string {
	flags := []struct {
		bit  uint32
		name string
	}{
		{0x0001, "PROCESSED_INPUT"}, // Ctrl+C 由系统处理，不返回给应用
		{0x0002, "LINE_INPUT"},      // 行缓冲：需要 Enter 才交付
		{0x0004, "ECHO_INPUT"},      // 控制台回显输入
		{0x0008, "WINDOW_INPUT"},    // 窗口尺寸变化进入输入队列
		{0x0010, "MOUSE_INPUT"},     // 鼠标事件进入输入队列
		{0x0020, "INSERT_MODE"},
		{0x0040, "QUICK_EDIT_MODE"},
		{0x0080, "EXTENDED_FLAGS"},
		{0x0200, "VIRTUAL_TERMINAL_INPUT"}, // VT 序列解析（现代终端的标志）
	}
	var out []string
	for _, f := range flags {
		if mode&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, "|")
}
