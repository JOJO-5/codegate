//go:build windows

package terminal

import (
	"os/exec"
	"syscall"
)

func detachTerminalTestChild(cmd *exec.Cmd) {
	// Deliberately detach from the console: closing ConPTY alone cannot own it.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}
