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

func TestLiveContinuationRequiresControlPreflightCapability(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "live-preflight-old@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	e.agentHandshake(t, device, priv, protocol.AgentCaps{BackgroundContinue: true, CodexLiveContinue: true})
	meta := &storage.SessionMeta{ID: uuid.NewString(), UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: "running", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(t.Context(), meta); err != nil {
		t.Fatal(err)
	}
	r := e.post(t, "/api/v1/devices/"+device+"/continuations", token, map[string]any{"session_id": meta.ID, "prompt": "continue", "mode": "live_quota", "due_at": e.clock.Now().UnixMilli(), "nonce": uuid.NewString()})
	if r.Status != 409 {
		t.Fatalf("unverified control service accepted: %d %s", r.Status, r.Body)
	}
	rows, err := e.store.Continuations(t.Context(), owner, device)
	if err != nil || len(rows) != 0 {
		t.Fatal("failed preflight created a task", rows, err)
	}
}

func TestCodexLiveContinuationPreflightRejectsThenRecoversWithoutCreatingFailures(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "live-preflight@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{CodexLiveContinue: true, LiveContinueCheck: true})
	meta := &storage.SessionMeta{ID: uuid.NewString(), UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: "running", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(t.Context(), meta); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/devices/" + device + "/continuations"
	nonce := uuid.NewString()
	body := map[string]any{"session_id": meta.ID, "prompt": "keep my draft", "mode": "live_quota", "due_at": e.clock.Now().UnixMilli(), "nonce": nonce}
	for _, state := range []string{"needs_review", "ready"} {
		response := make(chan *apiResponse, 1)
		go func() { response <- e.post(t, path, token, body) }()
		req := agentRecv(t, agent)
		if req.Type != protocol.TypeTaskCheck {
			t.Fatal("not a read-only preflight", req.Type)
		}
		p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
		if err != nil || p.Source.Recovery.NativeID != meta.Recovery.NativeID || p.Source.Cwd != meta.Cwd || p.UserID != owner || p.Prompt != "" || p.TaskID != "" {
			t.Fatal("preflight did not use owned source or carried execution", p, err)
		}
		// Preflight must not block unrelated task cancellation behind tasksMu.
		lockReady := make(chan struct{})
		go func() { e.srv.tasksMu.Lock(); e.srv.tasksMu.Unlock(); close(lockReady) }()
		select {
		case <-lockReady:
		case <-time.After(time.Second):
			t.Fatal("preflight blocked task cancellation")
		}
		agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{State: state, Detail: "restore original conversation"})
		r := <-response
		rows, err := e.store.Continuations(t.Context(), owner, device)
		if state == "needs_review" {
			if r.Status != 409 || err != nil || len(rows) != 0 {
				t.Fatal("failed preflight created a task", r.Status, rows, err)
			}
		} else {
			if r.Status != 201 || err != nil || len(rows) != 1 || rows[0].Prompt != "keep my draft" {
				t.Fatal("retry could not recover", r.Status, rows, err)
			}
			// Same nonce remains idempotent and does not send another check/run.
			if duplicate := e.post(t, path, token, body); duplicate.Status != 200 {
				t.Fatal("nonce replay", duplicate.Status)
			}
		}
	}
}

func TestConcurrentCodexPreflightsCreateOnlyOneMonitor(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "live-concurrent@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{CodexLiveContinue: true, LiveContinueCheck: true})
	meta := &storage.SessionMeta{ID: uuid.NewString(), UserID: owner, DeviceID: device, Command: "codex", Cwd: "/owned/work", Status: "running", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(t.Context(), meta); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *apiResponse, 2)
	for i := 0; i < 2; i++ {
		go func() {
			responses <- e.post(t, "/api/v1/devices/"+device+"/continuations", token, map[string]any{"session_id": meta.ID, "prompt": "continue", "mode": "live_quota", "due_at": e.clock.Now().UnixMilli(), "nonce": uuid.NewString()})
		}()
	}
	first, second := agentRecv(t, agent), agentRecv(t, agent)
	for _, req := range []*protocol.Envelope{first, second} {
		if req.Type != protocol.TypeTaskCheck {
			t.Fatal(req.Type)
		}
		agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{State: "ready"})
	}
	a, b := <-responses, <-responses
	if !(a.Status == 201 && b.Status == 409 || a.Status == 409 && b.Status == 201) {
		t.Fatal("concurrent duplicate monitors", a.Status, b.Status)
	}
	rows, err := e.store.Continuations(t.Context(), owner, device)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestContinuationDeletionPersistsAndPreservesNonce(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "delete-tasks@example.com", "correct-horse-battery")
	other := e.signup(t, "delete-other@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, _ := e.seedPairedDevice(t, owner)
	task := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: owner, DeviceID: device, State: "running", Attempt: 1}
	nonce := uuid.NewString()
	if err := e.store.InsertContinuation(t.Context(), task, nonce); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/devices/" + device + "/continuations"
	remove := path + "/" + task.ID
	if r := e.request(t, "DELETE", remove, other, nil); r.Status != 404 {
		t.Fatal("other owner deletion", r.Status)
	}
	if r := e.request(t, "DELETE", remove, token, nil); r.Status != 409 {
		t.Fatal("active task deletion must fail", r.Status)
	}
	task.State = "completed"
	if _, err := e.store.UpdateContinuation(t.Context(), task, "running"); err != nil {
		t.Fatal(err)
	}
	if r := e.request(t, "DELETE", remove, token, nil); r.Status != 200 {
		t.Fatal("delete", r.Status, string(r.Body))
	}
	for i := 0; i < 2; i++ {
		r := e.get(t, path, token)
		var body struct {
			Tasks []protocol.ContinuationTask `json:"tasks"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil || len(body.Tasks) != 0 {
			t.Fatal("deleted task reappeared", string(r.Body), err)
		}
	}
	old, err := e.store.ContinuationByNonce(t.Context(), owner, nonce)
	if err != nil || old.ID != task.ID {
		t.Fatal("lost execution idempotency", err)
	}
	if r := e.request(t, "DELETE", remove, token, nil); r.Status != 200 {
		t.Fatal("delete not idempotent", r.Status)
	}
	if r := e.post(t, remove+"/restore", token, nil); r.Status != 200 {
		t.Fatal("restore", r.Status)
	}
	rows, err := e.store.Continuations(t.Context(), owner, device)
	if err != nil || len(rows) != 1 || rows[0].State != "completed" {
		t.Fatal("restore changed execution", rows, err)
	}
}

func TestContinuationProgressRejectsLateSnapshots(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "progress-tasks@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, _ := e.seedPairedDevice(t, owner)
	task := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: owner, DeviceID: device, State: "running", Attempt: 1}
	if err := e.store.InsertContinuation(t.Context(), task, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	ac := &AgentConn{DeviceID: device, UserID: owner}
	apply := func(seq int64, text string) {
		e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "running", Progress: &protocol.TaskProgress{Sequence: seq, Text: text}})
	}
	apply(3, "new")
	apply(2, "old")
	got, err := e.store.ContinuationByID(t.Context(), task.ID)
	if err != nil || got.Progress == nil || got.Progress.Text != "new" {
		t.Fatal("late snapshot overwrote progress", got, err)
	}
	// A restarted Agent can confirm only that the previous process is unknown;
	// that status must not be dropped merely because no new tool event exists.
	e.srv.applyTaskState(t.Context(), ac, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "needs_review", Detail: "Agent restarted", Progress: &protocol.TaskProgress{Sequence: 3, Text: "new"}})
	got, err = e.store.ContinuationByID(t.Context(), task.ID)
	if err != nil || got.State != "needs_review" {
		t.Fatal("restart outcome hidden by snapshot deduplication", got, err)
	}
}

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

func TestOwnerCanCancelNeedsReviewContinuationWithoutRestartingSource(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "review-cancel@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, _ := e.seedPairedDevice(t, owner)
	task := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: owner, DeviceID: device, SessionID: uuid.NewString(), Mode: "live_quota", State: "needs_review", Detail: "identity mismatch", Attempt: 1, DueAt: e.clock.Now().UnixMilli()}
	if err := e.store.InsertContinuation(t.Context(), task, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/devices/" + device + "/continuations/" + task.ID + "/cancel"
	r := e.post(t, path, token, nil)
	if r.Status != 200 {
		t.Fatal(r.Status)
	}
	got, err := e.store.ContinuationByID(t.Context(), task.ID)
	if err != nil || got.State != "cancelled" || got.Attempt != 1 {
		t.Fatalf("cancel did not acknowledge blocked arrangement: %+v %v", got, err)
	}
	e.srv.pollContinuations(t.Context())
	got, _ = e.store.ContinuationByID(t.Context(), task.ID)
	if got.State != "cancelled" {
		t.Fatal("cancelled arrangement dispatched")
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

func TestLiveContinuationProviderPolicyAndDuplicateMonitor(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "live-policy@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{BackgroundContinue: true, CodexLiveContinue: true, LiveContinueCheck: true, LiveContinueProviders: []string{"codex", "claude", "opencode", "dsh"}})
	path := "/api/v1/devices/" + device + "/continuations"
	for _, cli := range []string{"codex", "claude", "opencode", "dsh"} {
		meta := &storage.SessionMeta{ID: uuid.NewString(), UserID: owner, DeviceID: device, Command: cli, Cwd: "/owned/work", Status: "running", Recovery: &protocol.ConversationBinding{CommandID: cli, NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
		if err := e.store.UpsertSession(t.Context(), meta); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"session_id": meta.ID, "prompt": "continue original task", "mode": "live_quota", "due_at": e.clock.Now().UnixMilli(), "nonce": uuid.NewString(), "max_retries": 2, "retry_errors": true, "quota_wait_minutes": 300}
		if cli == "codex" {
			go func() {
				req := agentRecv(t, agent)
				if req.Type != protocol.TypeTaskCheck {
					t.Error("missing read-only preflight", req.Type)
				}
				agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{State: "ready"})
			}()
		}
		response := e.post(t, path, token, body)
		if response.Status != 201 {
			t.Fatalf("%s: %d %s", cli, response.Status, response.Body)
		}
		var task protocol.ContinuationTask
		json.Unmarshal(response.Body, &task)
		if task.MaxRetries != 2 || !task.RetryErrors || task.QuotaWaitMinutes != 300 {
			t.Fatal(task)
		}
		body["nonce"] = uuid.NewString()
		if r := e.post(t, path, token, body); r.Status != 409 {
			t.Fatal("duplicate monitor", r.Status)
		}
		body["max_retries"] = 11
		if r := e.post(t, path, token, body); r.Status != 400 {
			t.Fatal("unbounded retries", r.Status)
		}
		done := make(chan struct{})
		go func() { e.srv.pollContinuations(t.Context()); close(done) }()
		req := agentRecv(t, agent)
		p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
		if err != nil || p.Mode != "live_quota" || p.MaxRetries != 2 || !p.RetryErrors || p.QuotaWaitMinutes != 300 {
			t.Fatal(p, err)
		}
		agentReply(t, agent, req, protocol.TypeTaskState, protocol.TaskStatePayload{TaskID: task.ID, Attempt: 1, State: "waiting_wall"})
		<-done
		r := e.post(t, path+"/"+task.ID+"/cancel", token, nil)
		if r.Status != 200 {
			t.Fatal(r.Status)
		}
	}
}
