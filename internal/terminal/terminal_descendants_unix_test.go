//go:build linux || darwin

package terminal

import "os/exec"

func detachTerminalTestChild(cmd *exec.Cmd) {}
