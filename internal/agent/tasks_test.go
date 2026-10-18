package agent

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestContinuationProgressProcessHelper(t *testing.T) {
	if os.Getenv("CODEGATE_PROGRESS_HELPER") != "1" {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	fmt.Println(`{"type":"item.started","item":{"type":"command_execution","command":"SECRET_COMMAND"}}`)
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"live reply before exit"}}`)
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestContinuationPublishesBeforeExitAndRetainsOnCancel(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, state := t.TempDir(), t.TempDir()
	native := uuid.NewString()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": native, "cwd": root}})
	if err := os.WriteFile(filepath.Join(home, "sessions", "rollout.jsonl"), append(header, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(root, name)
	if err := os.WriteFile(executable, binary, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig(t)
	cfg.StateDir = state
	cfg.AllowedRoots = []string{root}
	args := []string{"-test.run=^TestContinuationProgressProcessHelper$", "--"}
	cfg.AllowedCommands = []CommandSpec{{ID: "codex", Command: executable, Args: args, Kind: KindTUI}}
	cfg.Env = map[string]string{"CODEX_HOME": home, "CODEGATE_PROGRESS_HELPER": "1"}
	a, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer a.stopBackgroundTasks()
	p := protocol.TaskRunPayload{TaskID: uuid.NewString(), UserID: "alice", Attempt: 1, Mode: "time", Prompt: "continue", MaxRunMinutes: 1, Source: protocol.SessionSummary{SessionID: uuid.NewString(), Command: executable, Args: args, Cwd: root, Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native}}}
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskRun, "", p)
	a.onTaskRun(req)
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, e := a.taskReceipt(p.TaskID, 1, p.UserID)
		if e == nil && r.Result.Progress != nil && r.Result.Progress.Text == "live reply before exit" {
			if r.Result.State != "running" || r.Result.Progress.Sequence < 2 {
				t.Fatal("progress was only published after exit", r.Result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no live progress before exit", r.Result, e)
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskCancel, "", p)
	a.onTaskInspect(cancel, true)
	a.tasksWG.Wait()
	r, err := a.taskReceipt(p.TaskID, 1, p.UserID)
	if err != nil || r.Result.State != "cancelled" || r.Result.Progress.Text != "live reply before exit" || strings.Contains(r.Result.Progress.Activity, "SECRET") {
		t.Fatal("cancel lost safe live evidence", r.Result, err)
	}
}

func TestContinuationArgumentsAndInputIsolation(t *testing.T) {
	id := uuid.NewString()
	for _, tool := range []string{"codex", "claude", "opencode"} {
		args, e := continuationArgs(ResolvedCommand{Command: tool, Args: []string{"--model", "approved-model"}}, id)
		if e != nil || !strings.Contains(strings.Join(args, " "), id) || !strings.Contains(strings.Join(args, " "), "approved-model") {
			t.Fatalf("%s %v %v", tool, args, e)
		}
	}
	for _, arg := range []string{"--ephemeral", "--fork", "--fork-session", "resume", "--last"} {
		if _, e := continuationArgs(ResolvedCommand{Command: "codex", Args: []string{arg}}, id); e == nil {
			t.Fatal("unsafe config", arg)
		}
	}
	args, e := continuationArgs(ResolvedCommand{Command: "C:\\Windows\\System32\\cmd.exe", Args: []string{"/c", "C:\\tools\\codex.cmd", "--no-alt-screen"}}, id)
	if e != nil || args[0] != "/c" || args[1] != "C:\\tools\\codex.cmd" || strings.Contains(strings.Join(args, " "), "--no-alt-screen") {
		t.Fatal(args, e)
	}
}
func TestContinuationReceiptsPersistAndRejectOtherOwner(t *testing.T) {
	a := &Agent{cfg: Config{StateDir: t.TempDir()}}
	p := protocol.TaskRunPayload{TaskID: uuid.NewString(), UserID: "alice", Attempt: 1, Prompt: "continue $(danger) & 中文", Source: protocol.SessionSummary{Cwd: "work"}}
	r := taskReceipt{UserID: p.UserID, Hash: continuationHash(p), Result: protocol.TaskStatePayload{TaskID: p.TaskID, Attempt: 1, State: "preparing"}}
	if e := a.saveTaskReceipt(r); e != nil {
		t.Fatal(e)
	}
	// A restarted Agent sees the durable prepare marker, never treats it as a new run.
	restarted := &Agent{cfg: a.cfg}
	got, e := restarted.taskReceipt(p.TaskID, 1, "alice")
	if e != nil || got.Hash != r.Hash || got.Result.State != "preparing" {
		t.Fatal(got, e)
	}
	if _, e = restarted.taskReceipt(p.TaskID, 1, "bob"); e == nil {
		t.Fatal("other owner")
	}
	p.Prompt = "different"
	if continuationHash(p) == got.Hash {
		t.Fatal("changed prompt accepted")
	}
	r.Result.State = "completed"
	if e = a.saveTaskReceipt(r); e != nil {
		t.Fatal(e)
	}
	got, e = restarted.taskReceipt(p.TaskID, 1, "alice")
	if e != nil || got.Result.State != "completed" {
		t.Fatal(got, e)
	}
	if _, e = a.receiptPath("../../evil", 1); e == nil {
		t.Fatal("path traversal")
	}
}
func TestContinuationOutputKeepsOnlyFinalModelText(t *testing.T) {
	o := &taskOutput{}
	o.Write([]byte("{\"type\":\"tool\",\"result\":\"secret\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"text\":\"secret\"}}\n"))
	b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": strings.Repeat("中文", 5000)}})
	o.Write(b)
	text := o.Text()
	if len(text) > 16<<10 || strings.Contains(text, "secret") || !strings.HasPrefix(text, "中文") || !json.Valid([]byte(`"`+text+`"`)) {
		t.Fatal("output invalid")
	}
}

func TestContinuationProgressUsesSafeActivitiesAndCompleteLines(t *testing.T) {
	o := &taskOutput{}
	o.Write([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"SECRET_COMMAND","aggregated_output":"SECRET_OUTPUT"}}` + "\n"))
	text, activity, _ := o.Snapshot()
	if text != "" || activity != "正在执行命令" {
		t.Fatal("missing safe activity", text, activity)
	}
	o.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"working"}}`))
	text, _, _ = o.Snapshot()
	if text != "" {
		t.Fatal("partial line consumed")
	}
	o.Write([]byte("\n"))
	text, activity, at := o.Snapshot()
	if text != "working" || activity != "收到模型回复" || at == 0 {
		t.Fatal("missing live reply", text, activity, at)
	}
}

func TestContinuationSeparatesReportedSandboxFailureFromNormalExit(t *testing.T) {
	o := &taskOutput{}
	o.Write([]byte(`{"type":"item.completed","item":{"type":"command_execution","exit_code":-1,"aggregated_output":"helper_unknown_error: setup refresh had errors"}}` + "\n"))
	o.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"The environment is blocked."}}` + "\n"))
	if !o.EnvironmentBlocked() || o.Text() != "The environment is blocked." {
		t.Fatal("reported execution infrastructure failure was lost")
	}
	o.Write([]byte(`{"type":"item.completed","item":{"type":"command_execution","exit_code":0,"aggregated_output":"OK"}}` + "\n"))
	if o.EnvironmentBlocked() {
		t.Fatal("successful command did not acknowledge restored execution")
	}
	plain := &taskOutput{}
	plain.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"Explain helper_unknown_error: setup refresh had errors"}}` + "\n"))
	if plain.EnvironmentBlocked() {
		t.Fatal("plain model text became authoritative execution status")
	}
}
func TestContinuationWaitsForAllExhaustedWindows(t *testing.T) {
	now := time.Unix(1000, 0)
	q := protocol.QuotaResult{Providers: []protocol.ProviderQuota{{Status: "ok", Windows: []protocol.QuotaWindow{{UsedPercent: 100, ResetsAt: 2000}, {UsedPercent: 100, ResetsAt: 9000}, {UsedPercent: 1, ResetsAt: 10000}}}}}
	if blockedUntil(q, now) != 9030 {
		t.Fatal("ignored weekly wall")
	}
	q.Providers[0].Status = "unavailable"
	if blockedUntil(q, now) != 0 {
		t.Fatal("unknown quota guessed")
	}
}

func TestContinuationQuotaWindowsShimIsDefaultProfile(t *testing.T) {
	if !continuationQuotaEligible(ResolvedCommand{Command: "C:\\Windows\\cmd.exe", Args: []string{"/c", "codex.cmd"}}) {
		t.Fatal("default npm shim rejected")
	}
	if continuationQuotaEligible(ResolvedCommand{Command: "codex", Args: []string{"-c", "model_provider=other"}}) {
		t.Fatal("unknown profile quota accepted")
	}
}

// Exercise the production runner with a real owned process, not an invented
// success reply. Native vendor integration is additionally checked manually.
func TestContinuationRunnerDeduplicatesAndCancelsOwnedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture; Windows tree ownership is tested in processutil")
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	home, state := t.TempDir(), t.TempDir()
	native := uuid.NewString()
	os.MkdirAll(filepath.Join(home, "sessions"), 0700)
	header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": native, "cwd": root}})
	if e = os.WriteFile(filepath.Join(home, "sessions", "rollout.jsonl"), append(header, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	executable := filepath.Join(root, "codex")
	script := "#!/bin/sh\ncat > input.txt\nprintf started >> starts.txt\nsleep 30\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}'\n"
	if e = os.WriteFile(executable, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	cfg := DefaultConfig()
	cfg.Insecure = true
	cfg.ServerURL = "ws://127.0.0.1:1/api/v1/ws/agent"
	cfg.StateDir = state
	cfg.AllowedRoots = []string{root}
	cfg.AllowedCommands = []CommandSpec{{ID: "codex", Command: executable, Kind: KindTUI}}
	cfg.Env = map[string]string{"CODEX_HOME": home}
	a, e := New(cfg, testLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer a.stopBackgroundTasks()
	p := protocol.TaskRunPayload{TaskID: uuid.NewString(), UserID: "alice", Attempt: 1, Source: protocol.SessionSummary{SessionID: uuid.NewString(), Command: executable, Cwd: root, Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native}}, Prompt: "$(touch injected) & 中文", Mode: "time", MaxRunMinutes: 1}
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskRun, "", p)
	a.onTaskRun(req)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(root, "starts.txt")); string(b) == "started" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.onTaskRun(req)
	if b, _ := os.ReadFile(filepath.Join(root, "starts.txt")); string(b) != "started" {
		t.Fatal("duplicate model process", string(b))
	}
	input, _ := os.ReadFile(filepath.Join(root, "input.txt"))
	if string(input) != p.Prompt+"\n" {
		t.Fatal("prompt not stdin")
	}
	if _, e = os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(e) {
		t.Fatal("prompt executed as shell")
	}
	cancel, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskCancel, "", p)
	a.onTaskInspect(cancel, true)
	a.tasksWG.Wait()
	receipt, e := a.taskReceipt(p.TaskID, 1, "alice")
	if e != nil || receipt.Result.State != "cancelled" {
		t.Fatal(receipt.Result, e)
	}
	// Replay after completion remains cancelled and does not spawn again.
	a.onTaskRun(req)
	if b, _ := os.ReadFile(filepath.Join(root, "starts.txt")); string(b) != "started" {
		t.Fatal("replayed cancelled task")
	}
}
