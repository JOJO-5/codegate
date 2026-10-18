package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestOpenCodeV2UpdateWaitsForConfirmedNativeIdle(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/active" {
			select {
			case entered <- struct{}{}:
			default:
			}
			data := map[string]any{}
			if busy.Load() {
				data["ses_owned"] = map[string]any{"type": "running"}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		}
	}))
	defer server.Close()
	a := idleActivityAgent(t)
	finish, err := a.beginActivity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	client := testOpenCodeV2(t, server.URL, t.TempDir())
	go func() { a.releaseOpenCodeV2WhenIdle(ctx, client, "ses_owned", finish); close(done) }()
	<-entered
	if a.FreezeForUpdate() {
		t.Fatal("update ignored native background execution")
	}
	busy.Store(false)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed idle did not release update barrier")
	}
	if !a.FreezeForUpdate() {
		t.Fatal("idle shared service blocked update")
	}
}

func TestOpenCodeV2BackgroundTaskOutcomes(t *testing.T) {
	for _, scenario := range []string{"complete", "cancel", "lost-ack"} {
		t.Run(scenario, func(t *testing.T) {
			cwd := t.TempDir()
			var mu sync.Mutex
			own := "msg_before"
			active := false
			outcome := "succeeded"
			interrupts := 0
			prompts := 0
			admitted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/session/ses_owned/prompt":
					var body struct {
						ID   string `json:"id"`
						Text string `json:"text"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if body.Text != "literal & $(prompt) 中文" {
						t.Errorf("prompt changed: %q", body.Text)
					}
					prompts++
					own = body.ID
					active = scenario != "complete"
					close(admitted)
					if scenario == "lost-ack" {
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": own}})
				case "/api/session/ses_owned":
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_owned", "location": map[string]any{"directory": cwd}, "time": map[string]any{"idle": 100}, "outcome": outcome}})
				case "/api/session/ses_owned/message":
					item := map[string]any{"id": own, "type": "user", "time": map[string]any{"created": 10}}
					if r.URL.Query().Get("type") == "assistant" {
						item = map[string]any{"id": "msg_reply", "type": "assistant", "time": map[string]any{"created": 20}, "content": []any{map[string]any{"type": "text", "text": "verified output"}}}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": []any{item}})
				case "/api/session/active":
					data := map[string]any{}
					if active {
						data["ses_owned"] = map[string]any{"type": "running"}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
				case "/api/session/ses_owned/inbox":
					json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				case "/api/session/ses_owned/interrupt":
					interrupts++
					active = false
					outcome = "interrupted"
					json.NewEncoder(w).Encode(map[string]any{"interrupted": true})
				default:
					t.Errorf("unexpected path %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			parent, stopParent := context.WithCancel(t.Context())
			defer stopParent()
			a := &Agent{cfg: Config{StateDir: t.TempDir()}, log: testLogger(), quotaCtx: parent, tasks: map[string]*backgroundRun{}}
			p := protocol.TaskRunPayload{TaskID: uuid.NewString(), UserID: "owner", Attempt: 1, Prompt: "literal & $(prompt) 中文", Source: protocol.SessionSummary{Recovery: &protocol.ConversationBinding{NativeID: "ses_owned"}}}
			receipt := taskReceipt{UserID: p.UserID, Hash: continuationHash(p), Source: p.Source, Result: protocol.TaskStatePayload{TaskID: p.TaskID, Attempt: 1, State: "preparing"}}
			if err := a.saveTaskReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			run := &backgroundRun{receipt: receipt, cancel: cancel}
			key := taskRunKey(p.TaskID, 1)
			a.tasks[key] = run
			a.tasksWG.Add(1)
			finish, _ := a.beginActivity()
			req, _ := protocol.NewRequest("test", protocol.TypeTaskRun, "", p)
			a.startOpenCodeV2Task(req, p, receipt, run, key, testOpenCodeV2(t, server.URL, cwd), ctx, cancel, finish)
			select {
			case <-admitted:
			case <-time.After(3 * time.Second):
				t.Fatal("native prompt not admitted")
			}
			if scenario == "cancel" {
				cancel()
			}
			done := make(chan struct{})
			go func() { a.tasksWG.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("native task did not settle")
			}
			got, err := a.taskReceipt(p.TaskID, 1, p.UserID)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"complete": "completed", "cancel": "cancelled", "lost-ack": "needs_review"}[scenario]
			if got.Result.State != want {
				t.Fatalf("outcome %s, want %s: %s", got.Result.State, want, got.Result.Detail)
			}
			mu.Lock()
			defer mu.Unlock()
			if prompts != 1 || scenario == "cancel" && interrupts != 1 || scenario != "cancel" && interrupts != 0 {
				t.Fatalf("prompts=%d interrupts=%d", prompts, interrupts)
			}
			if scenario == "complete" && got.Result.Output != "verified output" {
				t.Fatal("missing native model output")
			}
			if scenario == "lost-ack" {
				if count, _ := a.activityState(); count != 1 {
					t.Fatal("ambiguous active native execution lost update barrier")
				}
			}
		})
	}
}
