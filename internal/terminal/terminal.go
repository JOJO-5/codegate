// Package terminal 是 PTY 的统一抽象层。
//
// 上层代码（session / agent）永远不应该知道自己是跑在 ConPTY 还是 Unix PTY 上。
// 平台差异全部收敛在 terminal_windows.go / terminal_unix.go 里。
//
// 设计文档：docs/PHASE0-ARCHITECTURE.md §6
package terminal

import (
	"context"
	"errors"
	"fmt"
)

// 默认伪控制台尺寸。
//
// 80x24 是终端世界的安全默认值：所有 TUI 都支持，不会触发极窄/极宽的
// 布局分支。前端 attach 之后会立刻用真实尺寸 resize 覆盖它。
const (
	DefaultCols uint16 = 80
	DefaultRows uint16 = 24
)

// Signal 是要投递给 PTY 前台进程的信号。
type Signal uint8

const (
	// SignalInterrupt 对应 Ctrl+C。
	//
	// ★ Windows 上它是唯一有真实实现的信号，且实现方式是"往 PTY 写入 0x03"，
	// 而不是调用某个系统 API —— ConPTY 没有公开的注入 Ctrl+C 的接口。
	// 实测（docs/PHASE0-R1-CONPTY-FINDINGS.md §7）：写 0x03 会被当作一个
	// 输入字符投递，不会产生 CTRL_C_EVENT。
	// 对 raw mode 的 TUI 来说这正是想要的（应用自己解释 Ctrl+C）；
	// 对依赖控制台中断语义的 shell，语义待 Phase 2 用 PowerShell/cmd 确认。
	SignalInterrupt Signal = iota + 1
	// SignalTerminate 对应 SIGTERM。
	SignalTerminate
	// SignalKill 对应 SIGKILL。
	SignalKill
)

func (s Signal) String() string {
	switch s {
	case SignalInterrupt:
		return "int"
	case SignalTerminate:
		return "term"
	case SignalKill:
		return "kill"
	default:
		return fmt.Sprintf("signal(%d)", uint8(s))
	}
}

// ParseSignal 把协议里的字符串转成 Signal。
func ParseSignal(s string) (Signal, error) {
	switch s {
	case "int", "interrupt", "sigint":
		return SignalInterrupt, nil
	case "term", "terminate", "sigterm":
		return SignalTerminate, nil
	case "kill", "sigkill":
		return SignalKill, nil
	default:
		return 0, fmt.Errorf("%w: 未知信号 %q", ErrUnsupported, s)
	}
}

var (
	// ErrUnsupported 表示当前平台不支持该操作。
	//
	// ★ 宁可明确报不支持，也不要假装成功。Windows 上没有 POSIX 信号，
	// 用假的成功返回值会让上层代码产生错误的假设。
	ErrUnsupported = errors.New("terminal: 当前平台不支持该操作")

	// ErrNotStarted 表示在 Start 之前调用了需要运行中会话的方法。
	ErrNotStarted = errors.New("terminal: 会话尚未启动")

	// ErrAlreadyStarted 表示重复 Start。
	ErrAlreadyStarted = errors.New("terminal: 会话已经启动")

	// ErrClosed 表示会话已关闭。
	ErrClosed = errors.New("terminal: 会话已关闭")
)

// StartConfig 是启动一个终端会话所需的全部输入。
type StartConfig struct {
	Command string
	Args    []string
	// Dir 是工作目录。必须在 Agent 的 allowed_workspaces 之内，
	// 校验由调用方（agent/workspace.go）负责 —— 本层不做安全判断。
	Dir string
	// Env 是完整的环境变量（"K=V" 形式），由 Agent 白名单构造。
	//
	// ★ 绝不来自网络，也绝不整体继承 Agent 自身环境。
	// 实测教训（F2）：不设 TERM，Codex 会直接拒绝进入 TUI；
	// 而整体继承又会把本机坏掉的 https_proxy 一起传下去。
	Env  []string
	Cols uint16
	Rows uint16
}

// ExitResult 描述进程的结束情况。
//
// 刻意把三种情况分开表达，而不是用一个 error 糊过去：
//
//	正常退出（含非 0 exit code）：Err == nil, ExitCode 有效, Signal == 0
//	被信号杀死：               Err == nil, Signal != 0
//	PTY 层故障：               Err != nil
//
// 原因：「进程以 exit code 1 正常结束」和「PTY 坏了」是完全不同的两件事，
// 用 error 表达会丢信息，调用方无法区分该重试还是该报错。
type ExitResult struct {
	ExitCode int
	Signal   Signal
	Err      error
}

// Exited 表示进程正常结束（不区分 exit code）。
func (r ExitResult) Exited() bool { return r.Err == nil && r.Signal == 0 }

// Success 表示进程正常结束且退出码为 0。
func (r ExitResult) Success() bool { return r.Exited() && r.ExitCode == 0 }

func (r ExitResult) String() string {
	switch {
	case r.Err != nil:
		return fmt.Sprintf("pty error: %v", r.Err)
	case r.Signal != 0:
		return fmt.Sprintf("killed by %s", r.Signal)
	default:
		return fmt.Sprintf("exit code %d", r.ExitCode)
	}
}

// Terminal 是 PTY 的统一接口（§6.1）。
//
// 接口刻意保持小：7 个方法，每个都有明确的多实现（Windows ConPTY / Unix PTY）。
// 不为"未来可能需要"增加方法 —— 规范第 40 条。
type Terminal interface {
	// Start 创建 PTY 并启动子进程。ctx 只用于启动阶段的取消，
	// 进程启动后的生命周期由 Close 控制。
	Start(ctx context.Context, cfg StartConfig) error

	// Read 返回 PTY 的原始字节。
	//
	// ★ 调用方不得假设返回值是完整的 UTF-8 字符或完整的转义序列 ——
	// 一个多字节字符完全可能被切在两次 Read 之间。这正是 §16.3
	// 要求终端流必须二进制透明的原因。
	Read(p []byte) (int, error)

	// Write 把字节写入 PTY 输入。
	Write(p []byte) (int, error)

	// Resize 调整伪控制台尺寸。内核/ConPTY 会通知前台进程组。
	Resize(cols, rows uint16) error

	// Signal 投递信号。不支持的信号返回 ErrUnsupported。
	Signal(sig Signal) error

	// Wait 阻塞直到进程结束并返回结果。
	//
	// ★ 调用时机很重要：必须在 Read 返回 EOF **之后**再调用，
	// 否则会丢掉子进程退出前最后写入的输出（§6.2 第 3 点）。
	Wait() ExitResult

	// Close 释放资源。必须幂等 —— 可能被 Close 路径和 defer 各调一次。
	Close() error

	// PID 返回子进程 ID。未启动时返回 0。
	PID() int
}
