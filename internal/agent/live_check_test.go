package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveContinuationPreflightIsReadOnlyAndRecoverable(t *testing.T) {
	a, _, p, f := liveFixture(t)
	if err := a.checkLiveContinuation(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.thread.Status.Type = "notLoaded"
	f.mu.Unlock()
	if err := a.checkLiveContinuation(t.Context(), p); err == nil || !strings.Contains(err.Error(), "恢复原对话") {
		t.Fatal("unloaded thread accepted or recovery missing", err)
	}
	f.mu.Lock()
	f.thread.Status.Type = "active"
	f.mu.Unlock()
	goodHome := a.cfg.Env["CODEX_HOME"]
	a.cfg.Env["CODEX_HOME"] = t.TempDir()
	if err := a.checkLiveContinuation(t.Context(), p); err == nil || !strings.Contains(err.Error(), "尚未开启") {
		t.Fatal("unavailable service accepted", err)
	}
	a.cfg.Env["CODEX_HOME"] = goodHome
	if err := a.checkLiveContinuation(t.Context(), p); err != nil {
		t.Fatal("readiness did not recover", err)
	}
	if _, err := a.taskReceipt(p.TaskID, p.Attempt, p.UserID); !os.IsNotExist(err) {
		t.Fatal("preflight persisted execution state", err)
	}
	if f.startCount() != 0 || f.interruptCount() != 0 {
		t.Fatal("preflight changed the model turn")
	}
}

func TestLiveContinuationPreflightRejectsWrongOwnerAndWorkspace(t *testing.T) {
	a, _, p, f := liveFixture(t)
	p.UserID = "other-owner"
	if err := a.checkLiveContinuation(t.Context(), p); err == nil {
		t.Fatal("wrong owner accepted")
	}
	// Restore the real owner, then make the control service report another cwd.
	current, _ := a.mgr.Get(parseUUID(p.Source.SessionID))
	p.UserID = current.UserID.String()
	f.mu.Lock()
	f.thread.Cwd = t.TempDir()
	f.mu.Unlock()
	if err := a.checkLiveContinuation(t.Context(), p); err == nil {
		t.Fatal("foreign workspace accepted")
	}
	if f.startCount() != 0 {
		t.Fatal("preflight submitted work")
	}
}

func TestCodexExplicitStandaloneAndRemoteCommandsArePreserved(t *testing.T) {
	a, _, _, _ := liveFixture(t)
	for _, args := range [][]string{{"--no-daemon"}, {"--remote", "ws://127.0.0.1:9999"}} {
		c := ResolvedCommand{Command: "codex", Args: append([]string{}, args...)}
		if err := a.prepareCodexDaemon(&c, BuildEnv(a.cfg, 80, 24), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if strings.Join(c.Args, " ") != strings.Join(args, " ") {
			t.Fatal("explicit transport overwritten", c.Args)
		}
	}
}

func TestCodexLivePreflightRefusesStandaloneAndCustomEndpoints(t *testing.T) {
	for _, args := range [][]string{{"--no-daemon"}, {"--remote", "ws://127.0.0.1:9999"}, {"--remote=unix://elsewhere"}} {
		a, sess, p, f := liveFixture(t)
		sess.Args = args
		if err := a.checkLiveContinuation(t.Context(), p); err == nil {
			t.Fatal("unsupported transport accepted", args)
		}
		if f.startCount() != 0 || f.interruptCount() != 0 {
			t.Fatal("preflight controlled a different service")
		}
	}
}

func TestCodexUnavailableStartupDoesNotSilentlyLaunchStandalone(t *testing.T) {
	a := &Agent{}
	c := ResolvedCommand{Command: filepath.Join(t.TempDir(), "missing-codex")}
	if err := a.prepareCodexDaemon(&c, []string{"CODEX_HOME=" + t.TempDir()}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "无法启动") {
		t.Fatal("startup failure was hidden", err)
	}
	if len(c.Args) != 0 {
		t.Fatal("failed startup changed terminal command", c.Args)
	}
}
