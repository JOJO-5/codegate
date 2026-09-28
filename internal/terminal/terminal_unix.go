//go:build !windows

package terminal

// 本文件是 Unix（Linux / macOS）上的占位实现。
//
// 开发计划里 Windows 是第一优先级（规范 §2），Unix PTY 排在 MVP 之后。
//
// ★ 这里刻意不做"看起来能用但没人验证过"的实现。
//   一个没跑过的 PTY 实现比明确报错更危险：上层会以为能用，
//   直到真正部署到 Linux 上才发现会话起不来，而那时排查成本高得多。
//   明确的错误至少能在启动时就暴露出来。
//
// 实现要点（留待后续阶段）：
//
//	open("/dev/ptmx") → grantpt → unlockpt → ptsname → 得到从端路径
//	子进程 setsid() 后 TIOCSCTTY 把从端设为控制终端（这一步不做，
//	  作业控制、Ctrl+C 的信号投递都不会工作）
//	resize 用 ioctl(fd, TIOCSWINSZ, &winsize)
//	Signal 直接用 syscall.Kill —— Unix 上信号是真实存在的，
//	  不需要像 Windows 那样"写 0x03"来模拟

import (
	"context"
	"errors"
)

// errUnixNotImplemented 表示 Unix PTY 还没做。
var errUnixNotImplemented = errors.New(
	"terminal: Unix PTY 尚未实现（开发计划中排在 Windows 之后）")

// Available 在 Unix 上始终返回"未实现"。
func Available() error { return errUnixNotImplemented }

// New 返回一个所有操作都会明确报错的终端。
func New() Terminal { return &unsupportedPTY{} }

type unsupportedPTY struct{}

func (u *unsupportedPTY) Start(context.Context, StartConfig) error { return errUnixNotImplemented }
func (u *unsupportedPTY) Read([]byte) (int, error)                 { return 0, errUnixNotImplemented }
func (u *unsupportedPTY) Write([]byte) (int, error)                { return 0, errUnixNotImplemented }
func (u *unsupportedPTY) Resize(uint16, uint16) error              { return errUnixNotImplemented }
func (u *unsupportedPTY) Signal(Signal) error                      { return errUnixNotImplemented }
func (u *unsupportedPTY) Wait() ExitResult                         { return ExitResult{Err: errUnixNotImplemented} }
func (u *unsupportedPTY) Close() error                             { return nil }
func (u *unsupportedPTY) PID() int                                 { return 0 }
