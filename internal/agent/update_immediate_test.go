package agent

import (
	"context"
	"errors"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestImmediateDrainCancelsOwnedOperationsAndBlocksNewWork(t *testing.T) {
	a := idleActivityAgent(t)
	ctx := a.operationContext()
	finish, err := a.beginActivity()
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); finish() }()
	deadline, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := a.drainForImmediateRestart(deadline, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.beginActivity(); !errors.Is(err, ErrUpdateBusy) {
		t.Fatal("new work entered during restart", err)
	}
	if n, _ := a.activityState(); n != 0 || a.mgr.Count() != 0 {
		t.Fatal("drain returned with live work", n)
	}
}

func TestImmediateRestartClosesRealOwnedTerminalAndPreservesFiles(t *testing.T) {
	if err := terminal.Available(); err != nil {
		t.Skip(err)
	}
	a := idleActivityAgent(t)
	a.cfg.StateDir = t.TempDir()
	root := t.TempDir()
	file := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(file, []byte("user code"), 0600); err != nil {
		t.Fatal(err)
	}
	a.mgr = session.NewManager(func(ctx context.Context, cfg terminal.StartConfig) (terminal.Terminal, error) {
		pty := terminal.New()
		if err := pty.Start(ctx, cfg); err != nil {
			return nil, err
		}
		return pty, nil
	}, session.Config{})
	t.Cleanup(func() { a.mgr.CloseAll("test complete") })
	command, args := "sh", []string{"-c", "sleep 120"}
	if runtime.GOOS == "windows" {
		command, args = "cmd.exe", []string{"/c", "ping -t 127.0.0.1 >nul"}
	}
	sess, err := a.mgr.Create(a.operationContext(), session.CreateRequest{Command: command, Args: args, Cwd: root, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Summary().PID <= 0 {
		t.Fatal("real owned child not started")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.drainForImmediateRestart(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("owned terminal remains alive")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "user code" {
		t.Fatal("code not preserved", err)
	}
}

func TestImmediateRestartValidatesConsentBeforeAnyShutdown(t *testing.T) {
	old := Version
	Version = "v1.0.0"
	defer func() { Version = old }()
	a := idleActivityAgent(t)
	a.log = testLogger()
	a.startedAt = time.Now()
	a.OnRestart = func() error { return nil }
	a.OnImmediateRestart = func(string, string) error { t.Fatal("shutdown before consent/ack"); return nil }
	for _, p := range []protocol.AgentUpdateControl{
		{Action: "restart_now", OwnerID: "owner", InstanceStartedAt: a.startedAt.UnixMilli()},
		{Action: "restart_now", OwnerID: "owner", Confirmed: true, InstanceStartedAt: a.startedAt.UnixMilli() - 1},
		{Action: "restart_now", OwnerID: "owner", Confirmed: true, InstanceStartedAt: a.startedAt.UnixMilli()},
	} {
		req, _ := protocol.NewRequest("force", protocol.TypeAgentUpdateControl, "", p)
		a.onUpdateControl(req) // no live connection: even valid consent has no acknowledgement
		if a.immediateRequested.Load() || a.drainingForUpdate() {
			t.Fatal("closed work without consent/ack")
		}
	}
}

func TestImmediateRestartStopsBoundNativeTurnAndPersistsCancellation(t *testing.T) {
	a, _, p, native := liveFixture(t)
	liveRun(t, a, p)
	restarts := 0
	a.OnImmediateRestart = func(path, version string) error {
		if a.mgr.Count() != 0 {
			t.Error("restart before terminal close")
		}
		if n, _ := a.activityState(); n != 0 {
			t.Error("restart before operation exit")
		}
		restarts++
		return nil
	}
	a.immediateRestart = &protocol.AgentUpdateControl{OwnerID: p.UserID}
	a.immediateRequested.Store(true)
	a.applyImmediateRestart(t.Context())
	a.applyImmediateRestart(t.Context())
	if restarts != 1 || native.interruptCount() != 1 || native.startCount() != 0 {
		t.Fatal("restart/interrupt/replay", restarts, native.interruptCount(), native.startCount())
	}
	r, err := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if err != nil || r.Result.State != "cancelled" {
		t.Fatal("missing durable cancellation", r.Result.State, err)
	}
	if a.currentUpdateStatus().State != "switching" {
		t.Fatal("switching not reported")
	}
}

func TestImmediateRestartDoesNotStopForeignNativeWorkspace(t *testing.T) {
	a, sess, _, native := liveFixture(t)
	source := sess.Summary()
	source.Cwd = t.TempDir()
	if err := a.stopNativeForImmediateRestart(t.Context(), source); err == nil {
		t.Fatal("foreign workspace accepted")
	}
	if native.interruptCount() != 0 {
		t.Fatal("interrupted foreign model")
	}
}

func TestImmediateRestartDoesNotClaimNewNativeTurnStopped(t *testing.T) {
	a, sess, _, native := liveFixture(t)
	native.mu.Lock()
	native.changeOnRecheck = true
	native.mu.Unlock()
	if err := a.stopNativeForImmediateRestart(t.Context(), sess.Summary()); err == nil {
		t.Fatal("new native turn reported stopped")
	}
}

func TestImmediateRestartOldMonitorCannotInterruptNewExternalTurn(t *testing.T) {
	a, sess, p, native := liveFixture(t)
	r := liveRun(t, a, p)
	r.Live.OwnTurnID = "old-owned-turn"
	if err := a.saveTaskReceipt(r); err != nil {
		t.Fatal(err)
	}
	if err := a.mgr.Close(sess.ID, "old terminal closed"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := a.drainForImmediateRestart(ctx, p.UserID); err != nil {
		t.Fatal(err)
	}
	if native.interruptCount() != 0 {
		t.Fatal("old receipt interrupted an unrelated new manual turn")
	}
	r, err := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if err != nil || r.Result.State != "cancelled" {
		t.Fatal("monitor arrangement not cancelled", err)
	}
}

func TestImmediateRestartFailedCheckLeavesWorkRunning(t *testing.T) {
	a, sess, p, native := liveFixture(t)
	a.cfg.ServerURL = "ws://localhost/unsupported"
	a.OnImmediateRestart = func(string, string) error { t.Fatal("restarted after failed check"); return nil }
	a.immediateRestart = &protocol.AgentUpdateControl{OwnerID: p.UserID}
	a.immediateRequested.Store(true)
	a.checkUpdate(t.Context())
	if a.mgr.Count() != 1 || a.drainingForUpdate() || native.interruptCount() != 0 {
		t.Fatal("failed check interrupted work")
	}
	if _, exists := a.mgr.Get(sess.ID); !exists {
		t.Fatal("lost original session")
	}
	if a.currentUpdateStatus().State != "error" {
		t.Fatal("check failure hidden")
	}
}

func TestImmediateRestartInspectionCannotOverwriteStopReceipt(t *testing.T) {
	a, _, p, native := liveFixture(t)
	before := liveRun(t, a, p)
	a.activityMu.Lock()
	a.updateDraining = true
	a.activityMu.Unlock()
	wire := &Conn{send: make(chan outbound, 16), closed: make(chan struct{})}
	a.setConn(wire)
	req, _ := protocol.NewRequest("inspect-draining", protocol.TypeTaskInspect, "", p)
	a.onTaskInspect(req, false)
	r, err := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if err != nil || r.Result.State != before.Result.State {
		t.Fatal("inspection changed durable stop state", err)
	}
	if native.interruptCount() != 0 || native.startCount() != 0 {
		t.Fatal("inspection controlled a model during restart")
	}
	raw := <-wire.send
	env, err := protocol.Decode(raw.data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocol.DecodePayload[protocol.TaskStatePayload](env)
	if err != nil || result.State != "cancelling" {
		t.Fatal("inspection did not report stopping", result.State, err)
	}
}

func TestImmediateDrainDoesNotClaimUnresponsiveOperationStopped(t *testing.T) {
	a := idleActivityAgent(t)
	finish, _ := a.beginActivity()
	defer finish()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := a.drainForImmediateRestart(ctx, "owner"); err == nil {
		t.Fatal("unresponsive operation reported stopped")
	}
}
