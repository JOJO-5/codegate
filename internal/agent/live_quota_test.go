package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
)

type nativeLiveFixture struct {
	mu                 sync.Mutex
	turn               nativeTurn
	thread             nativeThread
	allowed            any
	starts, interrupts int
	loseStart          bool
	changeOnRecheck    bool
	reads              int
	request            map[string]any
}

func (f *nativeLiveFixture) serve(w http.ResponseWriter, r *http.Request) {
	ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if e != nil {
		return
	}
	defer ws.Close()
	for {
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if ws.ReadJSON(&req) != nil {
			return
		}
		if req.ID == 0 {
			continue
		}
		f.mu.Lock()
		var out any = map[string]any{}
		switch req.Method {
		case "thread/read":
			out = map[string]any{"thread": f.thread}
		case "thread/turns/list":
			f.reads++
			if f.changeOnRecheck && f.reads == 2 {
				f.turn = nativeTurn{ID: uuid.NewString(), Status: "inProgress"}
			}
			out = map[string]any{"data": []nativeTurn{f.turn}}
		case "account/rateLimits/read":
			out = map[string]any{"ordinaryUsageAllowed": f.allowed}
		case "turn/start":
			f.starts++
			f.request = req.Params
			f.turn = nativeTurn{ID: uuid.NewString(), Status: "inProgress"}
			f.thread.Status.Type = "active"
			out = map[string]any{"turn": f.turn}
			if f.loseStart {
				f.mu.Unlock()
				return
			}
		case "turn/interrupt":
			f.interrupts++
			f.turn.Status = "interrupted"
		}
		raw, _ := json.Marshal(map[string]any{"id": req.ID, "result": out})
		f.mu.Unlock()
		if ws.WriteMessage(websocket.TextMessage, raw) != nil {
			return
		}
	}
}
func liveFixture(t *testing.T) (*Agent, *session.Session, protocol.TaskRunPayload, *nativeLiveFixture) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	// Keep Unix socket paths within their OS length limit, including Windows CI.
	home, e := os.MkdirTemp("", "cg-live-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	dir := filepath.Join(home, "app-server-control")
	if e = os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	sock := filepath.Join(dir, "app-server-control.sock")
	ln, e := net.Listen("unix", sock)
	if e != nil {
		t.Skipf("local Unix socket unavailable: %v", e)
	}
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.AllowedRoots = []string{root}
	cfg.Env = map[string]string{"CODEX_HOME": home}
	cfg.AllowedCommands = []CommandSpec{{ID: "codex", Command: "codex", Kind: KindTUI}}
	a := &Agent{log: testLogger(), cfg: cfg, ws: NewWorkspace(cfg.AllowedRoots)}
	a.mgr = session.NewManager(func(context.Context, terminal.StartConfig) (terminal.Terminal, error) {
		return &filesTestTerminal{done: make(chan struct{})}, nil
	}, session.Config{})
	t.Cleanup(func() { a.mgr.CloseAll("test") })
	t.Cleanup(a.stopBackgroundTasks)
	user := uuid.New()
	native := uuid.NewString()
	sess, e := a.mgr.Create(t.Context(), session.CreateRequest{DeviceID: uuid.New(), UserID: user, Command: "codex", Cwd: root, Cols: 80, Rows: 24, Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native}})
	if e != nil {
		t.Fatal(e)
	}
	f := &nativeLiveFixture{thread: nativeThread{ID: native, Cwd: root}, turn: nativeTurn{ID: uuid.NewString(), Status: "inProgress"}, allowed: false}
	f.thread.Status.Type = "active"
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	p := protocol.TaskRunPayload{TaskID: uuid.NewString(), UserID: user.String(), Attempt: 1, Source: sess.Summary(), Prompt: "继续原任务，检查已完成工作 $(not-shell) & 中文", Mode: "live_quota", MaxRunMinutes: 1}
	return a, sess, p, f
}
func liveRun(t *testing.T, a *Agent, p protocol.TaskRunPayload) taskReceipt {
	t.Helper()
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskRun, "", p)
	a.onTaskRun(req)
	r, e := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func setWall(f *nativeLiveFixture) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thread.Status.Type = "idle"
	f.turn.Status = "failed"
	f.turn.Error = &struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}{json.RawMessage(`"usageLimitExceeded"`)}
}
func TestLiveQuotaWaitsThenContinuesSameTerminalExactlyOnce(t *testing.T) {
	a, sess, p, f := liveFixture(t)
	r := liveRun(t, a, p)
	if r.Result.State != "waiting_wall" {
		t.Fatal(r.Result)
	}
	setWall(f)
	r.Live.NextQuotaAt = 0
	a.saveTaskReceipt(r)
	r = liveRun(t, a, p)
	if r.Result.State != "waiting_quota" || f.startCount() != 0 {
		t.Fatal(r.Result, f.startCount())
	}
	// Missing authoritative permission does not mean recovered.
	f.mu.Lock()
	f.allowed = nil
	f.mu.Unlock()
	r.Live.NextQuotaAt = 0
	a.saveTaskReceipt(r)
	r = liveRun(t, a, p)
	if r.Result.State != "waiting_quota" || f.startCount() != 0 {
		t.Fatal(r.Result)
	}
	f.mu.Lock()
	f.allowed = true
	f.mu.Unlock()
	r.Live.NextQuotaAt = 0
	a.saveTaskReceipt(r)
	r = liveRun(t, a, p)
	if r.Result.State != "running" || f.startCount() != 1 || sess.Summary().SessionID != p.Source.SessionID {
		t.Fatal(r.Result, f.startCount())
	}
	f.mu.Lock()
	request, _ := json.Marshal(f.request)
	f.mu.Unlock()
	var params map[string]any
	json.Unmarshal(request, &params)
	if params["threadId"] != p.Source.Recovery.NativeID || len(params) != 3 {
		t.Fatal("native identity/config override", params)
	}
	// Agent reconnect/replay uses the durable native turn receipt, not a second prompt.
	restarted := &Agent{log: testLogger(), cfg: a.cfg, ws: a.ws, mgr: a.mgr}
	r = liveRun(t, restarted, p)
	if f.startCount() != 1 || r.Live.OwnTurnID == "" {
		t.Fatal(r.Result, f.startCount())
	}
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeTaskCancel, "", p)
	restarted.onTaskInspect(req, true)
	r, _ = restarted.taskReceipt(p.TaskID, 1, p.UserID)
	if r.Result.State != "cancelled" || f.interruptCount() != 1 || !isRecoverableLive(sess.Summary()) {
		t.Fatal(r.Result, f.interruptCount())
	}
	liveRun(t, restarted, p)
	if f.startCount() != 1 {
		t.Fatal("cancel replay submitted")
	}
}
func TestLiveQuotaLostSubmissionNeverRepeats(t *testing.T) {
	a, _, p, f := liveFixture(t)
	liveRun(t, a, p)
	setWall(f)
	f.mu.Lock()
	f.allowed = true
	f.loseStart = true
	f.mu.Unlock()
	r := liveRun(t, a, p)
	if r.Result.State != "needs_review" || f.startCount() != 1 || !r.Live.Preparing {
		t.Fatal(r.Result, f.startCount())
	}
	liveRun(t, a, p)
	if f.startCount() != 1 {
		t.Fatal("lost response submitted twice")
	}
}
func TestLiveQuotaManualNewTurnWins(t *testing.T) {
	for _, recheck := range []bool{false, true} {
		t.Run(map[bool]string{false: "already_changed", true: "during_quota_query"}[recheck], func(t *testing.T) {
			a, _, p, f := liveFixture(t)
			liveRun(t, a, p)
			setWall(f)
			f.mu.Lock()
			f.allowed = true
			if recheck {
				f.changeOnRecheck = true
				f.reads = 0
			} else {
				f.turn.ID = uuid.NewString()
			}
			f.mu.Unlock()
			r := liveRun(t, a, p)
			if r.Result.State != "cancelled" || f.startCount() != 0 {
				t.Fatal(r.Result, f.startCount())
			}
		})
	}
}
func TestLiveQuotaOrdinaryErrorDoesNotResume(t *testing.T) {
	a, _, p, f := liveFixture(t)
	liveRun(t, a, p)
	setWall(f)
	f.mu.Lock()
	f.turn.Error.Info = json.RawMessage(`"rateLimitExceeded"`)
	f.allowed = true
	f.mu.Unlock()
	r := liveRun(t, a, p)
	if r.Result.State != "failed" || f.startCount() != 0 {
		t.Fatal(r.Result, f.startCount())
	}
}

func (f *nativeLiveFixture) startCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }
func (f *nativeLiveFixture) interruptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interrupts
}

func TestLiveQuotaSystemErrorAndConfiguredRetryCap(t *testing.T) {
	a, _, p, f := liveFixture(t)
	p.RetryErrors = true
	p.MaxRetries = 1
	liveRun(t, a, p)
	f.mu.Lock()
	f.thread.Status.Type = "systemError"
	f.turn.Status = "failed"
	f.turn.Error = &struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}{json.RawMessage(`"serverOverloaded"`)}
	f.mu.Unlock()
	r := liveRun(t, a, p)
	if r.Result.State != "waiting_retry" || f.startCount() != 0 {
		t.Fatal(r.Result)
	}
	r.Live.RetryAt = time.Now().Add(-time.Second).UnixMilli()
	if e := a.saveTaskReceipt(r); e != nil {
		t.Fatal(e)
	}
	r = liveRun(t, a, p)
	if r.Result.State != "running" || f.startCount() != 1 {
		t.Fatal(r.Result)
	}
	f.mu.Lock()
	f.turn.Status = "failed"
	f.turn.Error = &struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}{json.RawMessage(`"serverOverloaded"`)}
	f.thread.Status.Type = "systemError"
	f.mu.Unlock()
	r = liveRun(t, a, p)
	if r.Result.State != "failed" || f.startCount() != 1 {
		t.Fatal(r.Result)
	}
}

func TestLocalLiveRunLimitWorksWithoutServer(t *testing.T) {
	a, sess, p, f := liveFixture(t)
	liveRun(t, a, p)
	setWall(f)
	f.mu.Lock()
	f.allowed = true
	f.mu.Unlock()
	r := liveRun(t, a, p)
	r.Live.StartedAt = time.Now().Add(-2 * time.Minute).UnixMilli()
	if e := a.saveTaskReceipt(r); e != nil {
		t.Fatal(e)
	}
	if !a.inspectOwnedLiveRun(p, r.Live.OwnTurnID, false) {
		t.Fatal("expired turn still tracked")
	}
	r, _ = a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if r.Result.State != "failed" || f.interruptCount() != 1 || !isRecoverableLive(sess.Summary()) {
		t.Fatal(r.Result, f.interruptCount())
	}
}
