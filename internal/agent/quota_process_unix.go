//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

func configureQuotaProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func stopQuotaProcess(cmd *exec.Cmd) {
	// Native npm launchers spawn the real CLI; terminate the entire private group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
