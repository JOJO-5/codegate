//go:build !windows

package agent

import "os/exec"

func dshWebCommand(binary, host string) *exec.Cmd {
	return exec.Command(binary, "web", "--no-open", "--trusted-host", host)
}
