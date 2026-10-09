//go:build windows

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

func configureQuotaProcess(cmd *exec.Cmd) {}
func stopQuotaProcess(cmd *exec.Cmd) {
	// Kill the npm shim and its app-server child before closing the pipes.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	_ = cmd.Process.Kill()
}
