//go:build windows

package agent

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func dshWebCommand(binary, host string) *exec.Cmd {
	switch strings.ToLower(filepath.Ext(binary)) {
	case ".cmd", ".bat":
		// cmd /s /c requires outer quotes around a quoted executable path.
		// Host is validated before this call and cannot contain shell metacharacters.
		cmd := exec.Command("cmd.exe")
		// Go's normal argument escaping targets CommandLineToArgvW, not cmd.exe.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c ""` + binary + `" web --no-open --trusted-host "` + host + `""`}
		return cmd
	default:
		return exec.Command(binary, "web", "--no-open", "--trusted-host", host)
	}
}
