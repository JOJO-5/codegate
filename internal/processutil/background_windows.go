//go:build windows

// Package processutil configures non-PTY helper processes.
package processutil

import (
	"os/exec"
	"syscall"
)

// Background prevents Windows from allocating a console for a helper, including
// when its parent runs without a console. Preserve existing process attributes.
// ConPTY processes intentionally use their separate native launch path.
func Background(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
