//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// winsize 对应 <termios.h> 里的 struct winsize。
//
// 四个字段都是 uint16 —— 别改成 int，ioctl 是按这个内存布局读写的，
// 改宽度会让内核把相邻字段当成本结构的一部分。
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// consoleSize 用 TIOCGWINSZ 取终端窗口尺寸。
func consoleSize() (int, int, error) {
	ws := &winsize{}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if errno != 0 {
		return 0, 0, errno
	}
	return int(ws.Col), int(ws.Row), nil
}

// watchSize 用 SIGWINCH 感知窗口变化。
//
// 与 Windows 侧轮询不同，Unix 上内核会在 TIOCSWINSZ 之后主动给
// 前台进程组发 SIGWINCH —— 这是真正的「事件」，没有轮询延迟，
// 也不会漏掉中间态。
//
// ⚠️ 本分支**尚未在本机验证**：开发机是 Windows，且 internal/terminal
// 的 Unix 实现目前是明确报错的占位（terminal_unix.go）。
// 等 Unix PTY 落地后，这个函数要和它一起过一遍真实 PTY。
func watchSize(stop <-chan struct{}, onChange func(cols, rows int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-stop:
			return
		case <-ch:
			if c, r, err := consoleSize(); err == nil {
				onChange(c, r)
			}
		}
	}
}
