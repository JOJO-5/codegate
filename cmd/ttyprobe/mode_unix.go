//go:build !windows

package main

import "errors"

// Unix 上没有"控制台模式"这个概念 —— termios 的 ICANON/ECHO/ISIG
// 是另一套模型，而且由**子进程自己**通过 tcsetattr 控制。
// 这里明确返回不支持，而不是编一个看起来像的映射出来。
var errNoConsoleMode = errors.New("非 Windows 平台无控制台模式概念")

func stdinConsoleMode() (uint32, error) { return 0, errNoConsoleMode }

func describeInputMode(uint32) string { return "(n/a)" }

// setRawMode 在 Unix 上需要 tcgetattr/tcsetattr 操作 termios，
// 与 Windows 的控制台模式是两套完全不同的模型。
// Unix PTY 尚未实现（见 internal/terminal/terminal_unix.go），
// 这里明确报不支持，而不是写一个没人验证过的实现。
func setRawMode() error { return errNoConsoleMode }
