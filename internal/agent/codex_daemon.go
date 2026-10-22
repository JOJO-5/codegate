package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/processutil"
)

// An embedded TUI server is invisible to the shared control socket. Explicitly
// connect new terminals to the same service that continuation will inspect.
// Starting an absent daemon is idempotent; never restart/stop a shared service.
func (a *Agent) prepareCodexDaemon(c *ResolvedCommand, env []string, cwd string) error {
	defaultRemote := false
	for i, arg := range c.Args {
		if arg == "--no-daemon" {
			return nil // Preserve an explicitly requested standalone terminal.
		}
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") {
			if arg == "--remote=unix://" || arg == "--remote" && i+1 < len(c.Args) && c.Args[i+1] == "unix://" {
				defaultRemote = true
			} else {
				return nil
			} // Never replace a custom endpoint.
		}
	}
	a.codexStartMu.Lock()
	defer a.codexStartMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ensureCodexDaemon(ctx, *c, env, cwd); err != nil {
		return err
	}
	if !defaultRemote {
		c.Args = append(c.Args, "--remote", "unix://")
	}
	return nil
}

func ensureCodexDaemon(ctx context.Context, c ResolvedCommand, env []string, cwd string) error {
	probe := func() error {
		check, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		control, err := openCodexControl(check, codexHome(env))
		if err == nil {
			control.close()
		}
		return err
	}
	err := probe()
	if err == nil {
		return nil
	}
	var unavailable *codexControlUnavailable
	if !errors.As(err, &unavailable) {
		return err
	}
	args := []string{}
	if strings.EqualFold(filepath.Base(c.Command), "cmd.exe") && len(c.Args) >= 2 && strings.EqualFold(c.Args[0], "/c") {
		args = append(args, c.Args[:2]...)
	}
	args = append(args, "app-server", "daemon", "start")
	cmd := exec.CommandContext(ctx, c.Command, args...)
	cmd.Env, cmd.Dir = env, cwd
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	processutil.Background(cmd)
	// A concurrent local CLI may win startup. The handshake, not the exit code,
	// proves readiness. No prompt, turn, credentials or native history is sent.
	_ = cmd.Run()
	if probe() != nil {
		return errors.New("无法启动 Codex 本机控制服务；请更新 Codex，并在本机运行 codex app-server daemon start 后重试，原会话保留")
	}
	return nil
}
