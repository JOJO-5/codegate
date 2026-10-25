package agent

import (
	"context"
	"errors"
	"io"
	"os"
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
	var diagnostics codexDaemonDiagnostics
	cmd.Stdout, cmd.Stderr = io.Discard, &diagnostics
	processutil.Background(cmd)
	// A concurrent local CLI may win startup. The handshake, not the exit code,
	// proves readiness. No prompt, turn, credentials or native history is sent.
	startErr := cmd.Run()
	if probe() != nil {
		return codexDaemonStartupFailure(ctx.Err(), startErr, diagnostics.String())
	}
	return nil
}

// Keep only bounded, transient stderr for classification. Never forward raw CLI
// output: it can contain paths, credentials or user-provided configuration.
type codexDaemonDiagnostics struct{ text strings.Builder }

func (d *codexDaemonDiagnostics) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 16384 - d.text.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = d.text.Write(p)
	}
	return n, nil
}

func (d *codexDaemonDiagnostics) String() string { return d.text.String() }

func codexDaemonStartupFailure(ctxErr, startErr error, diagnostic string) error {
	reason := "已执行启动命令，但控制服务尚不可连接；请在本机检查 codex app-server daemon version 后重试"
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		reason = "启动等待超时；请检查本机 Codex 控制服务状态后重试"
	case strings.Contains(diagnostic, "this Windows launcher prevents background processes from outliving it"):
		reason = "后台启动器阻止控制服务独立运行；请更新后台启动器，在会话空闲后重启 Agent 并重试"
	case errors.Is(startErr, os.ErrNotExist), errors.Is(startErr, exec.ErrNotFound):
		reason = "未找到配置的 Codex 启动程序；请检查命令配置后重试"
	case errors.Is(startErr, os.ErrPermission):
		reason = "启动被系统拒绝；请检查本机权限和后台启动器后重试"
	case startErr != nil:
		reason = "启动命令执行失败；请在本机检查 codex app-server daemon start 的结果后重试"
	}
	return errors.New("无法启动 Codex 本机控制服务：" + reason + "，原会话保留")
}
