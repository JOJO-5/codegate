package server

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
	"testing"
	"time"
)

func TestContinuationAuthorizationPersistenceAndOffline(t *testing.T) {
	e := newTestEnv(t)
	alice := e.signup(t, "tasks-alice@example.com", "correct-horse-battery")
	bob := e.signup(t, "tasks-bob@example.com", "correct-horse-battery")
	owner := e.meUserID(t, alice)
	device, _ := e.seedPairedDevice(t, owner)
	sid, native := uuid.NewString(), uuid.NewString()
	meta := &storage.SessionMeta{ID: sid, UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: "terminated", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(t.Context(), meta); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/devices/" + device + "/continuations"
	nonce := uuid.NewString()
	body := map[string]any{"session_id": sid, "native_id": native, "prompt": "continue", "mode": "time", "due_at": e.clock.Now().UnixMilli(), "nonce": nonce, "source": map[string]string{"command": "evil", "cwd": "/outside"}}
	if r := e.post(t, path, bob, body); r.Status != 404 {
		t.Fatalf("cross owner %d", r.Status)
	}
	if r := e.post(t, path, alice, body); r.Status != 400 {
		t.Fatal("forged source accepted")
	}
	delete(body, "source")
	r := e.post(t, path, alice, body)
	if r.Status != 201 {
		t.Fatalf("create %d %s", r.Status, r.Body)
	}
	var task protocol.ContinuationTask
	if err := json.Unmarshal(r.Body, &task); err != nil {
		t.Fatal(err)
	}
	if task.Source.Command != "codex" || task.Source.Cwd != meta.Cwd || task.Source.Recovery.NativeID != native {
		t.Fatal("browser substituted source")
	}
	if r = e.post(t, path, alice, body); r.Status != 200 {
		t.Fatal("retry not idempotent", r.Status)
	}
	e.srv.pollContinuations(context.Background())
	got, err := e.store.ContinuationByID(t.Context(), task.ID)
	if err != nil || got.State != "waiting_agent" || got.Attempt != 0 {
		t.Fatal(got, err)
	}
	if r = e.post(t, path+"/"+task.ID+"/cancel", bob, nil); r.Status != 404 {
		t.Fatal("cross owner cancel")
	}
	if r = e.post(t, path+"/"+task.ID+"/cancel", alice, nil); r.Status != 200 {
		t.Fatal(r.Status)
	}
	e.clock.Advance(time.Minute * 2)
	e.srv.pollContinuations(t.Context())
	got, _ = e.store.ContinuationByID(t.Context(), task.ID)
	if got.State != "cancelled" {
		t.Fatal("cancelled task launched")
	}
}
func TestContinuationDispatchReceiptAndLateReply(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "task-dispatch@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{BackgroundContinue: true})
	sid := uuid.NewString()
	meta := &storage.SessionMeta{ID: sid, UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: "terminated", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(t.Context(), meta); err != nil {
		t.Fatal(err)
	}
	task := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: owner, DeviceID: device, SessionID: sid, Source: sessionSummary(meta), Prompt: "continue", Mode: "time", DueAt: e.clock.Now().UnixMilli(), State: "queued", MaxAttempts: 3, MaxRunMinutes: 1}
	if err := e.store.InsertContinuation(t.Context(), task, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.srv.pollContinuations(context.Background()); close(done) }()
	req := agentRecv(t, agent)
	if req.Type != protocol.TypeTaskRun {
		t.Fatal(req.Type)
	}
	p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
	if err != nil || p.Source.Recovery.NativeID != meta.Recovery.NativeID || p.Prompt != "continue" {
		t.Fatal(p, err)
	}
	agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "running"})
	<-done
	ac, _ := e.srv.reg.Agent(device)
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 2, State: "completed"})
	got, _ := e.store.ContinuationByID(t.Context(), task.ID)
	if got.State != "running" {
		t.Fatal("foreign attempt")
	}
	// A reconstructed Server checks an existing attempt rather than resending its prompt.
	doneChan := make(chan struct{})
	e.clock.Advance(time.Minute)
	go func() { e.srv.pollContinuations(context.Background()); close(doneChan) }()
	inspect := agentRecv(t, agent)
	if inspect.Type != protocol.TypeTaskInspect {
		t.Fatal("resent task", inspect.Type)
	}
	agentReply(t, agent, inspect, protocol.TypeTaskState, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "completed", Output: "done"})
	<-doneChan
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "running"})
	got, _ = e.store.ContinuationByID(t.Context(), task.ID)
	if got.State != "completed" || got.Output != "done" {
		t.Fatal("late reply regressed terminal state", got)
	}
}
func TestContinuationQuotaRetryBoundedAndCancellationWins(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "task-retry@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, _ := e.seedPairedDevice(t, owner)
	ac := &AgentConn{UserID: owner, DeviceID: device}
	row := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: owner, DeviceID: device, State: "running", Attempt: 1, MaxAttempts: 3}
	if err := e.store.InsertContinuation(t.Context(), row, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: row.ID, Attempt: 1, State: "waiting_quota", Retry: true, DueAt: e.clock.Now().Add(5 * time.Hour).UnixMilli()})
	got, _ := e.store.ContinuationByID(t.Context(), row.ID)
	if got.State != "waiting_quota" || !got.Retry || got.DueAt != e.clock.Now().Add(5*time.Hour).UnixMilli() {
		t.Fatal(got)
	}
	old := got.State
	got.State = "running"
	got.Attempt = 3
	if _, err := e.store.UpdateContinuation(t.Context(), got, old); err != nil {
		t.Fatal(err)
	}
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: row.ID, Attempt: 3, State: "waiting_quota", Retry: true})
	got, _ = e.store.ContinuationByID(t.Context(), row.ID)
	if got.State != "failed" {
		t.Fatal("unbounded retry", got)
	}
	old = got.State
	got.State = "cancelling"
	got.Attempt = 2
	e.store.UpdateContinuation(t.Context(), got, old)
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: row.ID, Attempt: 2, State: "waiting_quota", Retry: true})
	got, _ = e.store.ContinuationByID(t.Context(), row.ID)
	if got.State != "cancelling" {
		t.Fatal("late waiting overwrote cancellation")
	}
}

func TestContinuationCanBeScheduledWhileTerminalIsLive(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "live-schedule@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{BackgroundContinue: true})
	for _, status := range []string{"running", "starting", "detached"} {
		meta := &storage.SessionMeta{ID: uuid.NewString(), UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: status, Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
		if err := e.store.UpsertSession(t.Context(), meta); err != nil {
			t.Fatal(err)
		}
		response := e.post(t, "/api/v1/devices/"+device+"/continuations", token, map[string]any{"session_id": meta.ID, "prompt": "continue", "mode": "time", "due_at": e.clock.Now().UnixMilli(), "nonce": uuid.NewString()})
		if response.Status != 201 {
			t.Fatalf("%s: %d %s", status, response.Status, response.Body)
		}
		var task protocol.ContinuationTask
		if err := json.Unmarshal(response.Body, &task); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { e.srv.pollContinuations(t.Context()); close(done) }()
		req := agentRecv(t, agent)
		agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "waiting_idle", DueAt: e.clock.Now().Add(time.Minute).UnixMilli(), Detail: "原对话仍有终端运行"})
		<-done
		got, err := e.store.ContinuationByID(t.Context(), task.ID)
		if err != nil || got.State != "waiting_idle" {
			t.Fatalf("%s: %#v %v", status, got, err)
		}
	}
}
