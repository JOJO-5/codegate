package agent

// Opt-in protocol investigation against an installed Codex. The provider, home,
// project and daemon are owned by this test; no production session is attached.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/processutil"
)

type codexProbeFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// This deliberately does not use codexControl.call: notifications AND native
// server requests must survive while waiting for a client request's response.
// It is a bounded, single-threaded investigation client, not the Chat adapter.
type codexProbe struct {
	t       *testing.T
	control *codexControl
	next    int
	pending []codexProbeFrame
}

func (p *codexProbe) read() codexProbeFrame {
	p.t.Helper()
	var f codexProbeFrame
	if err := p.control.conn.ReadJSON(&f); err != nil {
		methods := make([]string, 0, len(p.pending))
		for _, event := range p.pending {
			methods = append(methods, event.Method)
		}
		p.t.Fatalf("native probe read: %v; queued methods: %v", err, methods)
	}
	return f
}

func (p *codexProbe) call(method string, params any, out any) {
	p.t.Helper()
	p.next++
	id := fmt.Sprint(p.next)
	if err := p.control.conn.WriteJSON(map[string]any{"id": p.next, "method": method, "params": params}); err != nil {
		p.t.Fatal(err)
	}
	for {
		f := p.read()
		if f.Method == "" && string(f.ID) == id {
			if len(f.Error) > 0 && string(f.Error) != "null" {
				p.t.Fatalf("isolated native %s: %s", method, f.Error)
			}
			if err := json.Unmarshal(f.Result, out); err != nil {
				p.t.Fatal(err)
			}
			return
		}
		p.pending = append(p.pending, f)
		if len(p.pending) > 4096 {
			p.t.Fatal("probe event limit exceeded")
		}
	}
}

func (p *codexProbe) event(method string, match func(json.RawMessage) bool) codexProbeFrame {
	p.t.Helper()
	for {
		for i, f := range p.pending {
			if f.Method == method && (match == nil || match(f.Params)) {
				p.pending = append(p.pending[:i], p.pending[i+1:]...)
				return f
			}
		}
		p.pending = append(p.pending, p.read())
		if len(p.pending) > 4096 {
			p.t.Fatal("probe event limit exceeded")
		}
	}
}

type codexChatFixture struct {
	a          *Agent
	home, root string
	env        []string
	mode       atomic.Int32
	posts      atomic.Int32
}

func newCodexChatFixture(t *testing.T) *codexChatFixture {
	t.Helper()
	binary := os.Getenv("CODEGATE_CODEX_CHAT_SMOKE")
	if binary == "" {
		t.Skip("set CODEGATE_CODEX_CHAT_SMOKE to the installed Codex executable")
	}
	f := &codexChatFixture{root: t.TempDir()}
	var err error
	f.home, err = os.MkdirTemp("", "cg-chat-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(f.home) })
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[]}`)
			return
		}
		f.posts.Add(1)
		// Drain fixture prompts, but never log or retain request bodies.
		io.Copy(io.Discard, io.LimitReader(r.Body, 4<<20))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		emit := func(kind string, payload map[string]any) {
			payload["type"] = kind
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
			w.(http.Flusher).Flush()
		}
		emit("response.created", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
		mode := f.mode.Load()
		if mode == 1 {
			// Deliberately stalled response for turn/interrupt, with no command.
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		if mode == 2 || mode == 3 {
			name := "exec_command"
			args := `{"cmd":"echo CodeGate isolated rejected action","sandbox_permissions":"require_escalated","justification":"CodeGate isolated approval probe"}`
			if mode == 3 {
				name = "request_user_input"
				args = `{"questions":[{"id":"fixture_choice","header":"Fixture","question":"Choose an isolated fixture answer","options":[{"label":"Continue","description":"Continue only the isolated fixture"},{"label":"Stop","description":"Stop the isolated fixture"}]}]}`
			}
			item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": name, "arguments": args}
			emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
			emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
			emit("response.completed", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
			return
		}
		item := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{}}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		emit("response.content_part.added", map[string]any{"item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": part})
		emit("response.output_text.delta", map[string]any{"item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "CodeGate isolated "})
		emit("response.output_text.delta", map[string]any{"item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "native chat marker"})
		part["text"] = "CodeGate isolated native chat marker"
		item["content"] = []any{part}
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		emit("response.completed", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	t.Cleanup(func() {
		close(release)
		provider.CloseClientConnections()
		provider.Close()
	})
	config := fmt.Sprintf("model = \"fixture\"\nmodel_provider = \"fixture\"\napproval_policy = \"on-request\"\nsandbox_mode = \"workspace-write\"\n[model_providers.fixture]\nname = \"Local isolated Chat test\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = \"CODEGATE_LOCAL_FAKE_KEY\"\n", provider.URL+"/v1")
	config += fmt.Sprintf("\n[projects.%q]\ntrust_level = \"trusted\"\n", f.root)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ServerURL: "https://localhost.invalid", StateDir: t.TempDir(), AllowedRoots: []string{f.root}, Env: map[string]string{"CODEX_HOME": f.home, "CODEGATE_LOCAL_FAKE_KEY": "local-dummy-no-credentials"}, AllowedCommands: []CommandSpec{{ID: "codex", Command: binary, Kind: KindTUI, Args: []string{"-s", "read-only", "-a", "never"}}}}
	f.env = BuildEnv(cfg, 100, 30)
	f.a, err = New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(ctx, binary, "app-server", "daemon", "stop")
		stop.Env = f.env
		processutil.Background(stop)
		if err := stop.Run(); err != nil {
			t.Error("isolated Chat daemon cleanup", err)
		}
	})
	t.Cleanup(func() { f.a.mgr.CloseAll("isolated Chat probe complete") })
	c := ResolvedCommand{Command: binary}
	if err := f.a.prepareCodexDaemon(&c, f.env, f.root); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *codexChatFixture) connect(t *testing.T) *codexProbe {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	t.Cleanup(cancel)
	c, err := openCodexControl(ctx, f.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	return &codexProbe{t: t, control: c, next: 100}
}

type codexProbeThread struct {
	Reviewer       string          `json:"approvalsReviewer"`
	Thread         nativeThread    `json:"thread"`
	ApprovalPolicy string          `json:"approvalPolicy"`
	Sandbox        json.RawMessage `json:"sandbox"`
}

func TestCodexChatNativeProtocolSmoke(t *testing.T) {
	f := newCodexChatFixture(t)
	p := f.connect(t)
	var started codexProbeThread
	p.call("thread/start", map[string]any{"cwd": f.root, "model": "fixture", "modelProvider": "fixture", "approvalPolicy": "never", "sandbox": "read-only", "allowProviderModelFallback": false, "historyMode": "paginated"}, &started)
	if started.Thread.ID == "" || started.ApprovalPolicy != "never" || !strings.Contains(string(started.Sandbox), `"readOnly"`) {
		t.Fatal("native startup policy mismatch")
	}
	id := started.Thread.ID
	var turn struct {
		Turn nativeTurn `json:"turn"`
	}
	p.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "isolated fixture prompt"}}}, &turn)
	if turn.Turn.ID == "" {
		t.Fatal("native turn identity missing")
	}
	var text strings.Builder
	for text.Len() < len("CodeGate isolated native chat marker") {
		e := p.event("item/agentMessage/delta", nil)
		var delta struct{ ThreadID, TurnID, ItemID, Delta string }
		if err := json.Unmarshal(e.Params, &delta); err != nil || delta.ThreadID != id || delta.TurnID != turn.Turn.ID || delta.ItemID == "" {
			t.Fatal("native delta identity mismatch", err)
		}
		text.WriteString(delta.Delta)
	}
	completed := p.event("turn/completed", nil)
	if !strings.Contains(string(completed.Params), `"completed"`) {
		t.Fatalf("fixture turn did not complete: %s", completed.Params)
	}
	if text.String() != "CodeGate isolated native chat marker" {
		t.Fatal("fixture streaming content mismatch")
	}
	// Reconnect with the native identity and read paginated persisted history.
	p.control.close()
	p = f.connect(t)
	var resumed codexProbeThread
	p.call("thread/resume", map[string]any{"threadId": id, "excludeTurns": true}, &resumed)
	if resumed.Thread.ID != id {
		t.Fatal("reconnect changed native identity")
	}
	var history json.RawMessage
	p.call("thread/turns/list", map[string]any{"threadId": id, "limit": 1, "itemsView": "notLoaded"}, &history)
	if !strings.Contains(string(history), turn.Turn.ID) {
		t.Fatal("reconnect lost turn history")
	}
	p.call("thread/items/list", map[string]any{"threadId": id, "turnId": turn.Turn.ID, "limit": 20}, &history)
	if !strings.Contains(string(history), text.String()) {
		t.Fatal("persisted items missing streamed message")
	}
	if f.posts.Load() != 1 {
		t.Fatal("reconnect replayed model request")
	}
	f.mode.Store(1)
	p.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "isolated stalled request"}}}, &turn)
	deadline := time.Now().Add(5 * time.Second)
	for f.posts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.posts.Load() != 2 {
		t.Fatal("stalled turn never reached isolated provider")
	}
	var raw json.RawMessage
	p.call("turn/interrupt", map[string]any{"threadId": id, "turnId": turn.Turn.ID}, &raw)
	interrupted := p.event("turn/completed", nil)
	if !strings.Contains(string(interrupted.Params), `"interrupted"`) || !strings.Contains(string(interrupted.Params), turn.Turn.ID) {
		t.Fatalf("interrupt not confirmed: %s", interrupted.Params)
	}
	t.Log("real CLI + local fixture: native thread/turn/item IDs, streaming, persisted pagination, reconnect without replay and confirmed interrupt passed; external model calls=0")
}

func TestCodexChatNativeRequestsSmoke(t *testing.T) {
	f := newCodexChatFixture(t)
	p := f.connect(t)
	var started codexProbeThread
	p.call("thread/start", map[string]any{"cwd": f.root, "model": "fixture", "modelProvider": "fixture", "approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": "read-only", "historyMode": "paginated"}, &started)
	id := started.Thread.ID
	var turn struct {
		Turn nativeTurn `json:"turn"`
	}
	f.mode.Store(2)
	p.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "isolated denied command probe"}}}, &turn)
	approval := p.event("item/commandExecution/requestApproval", nil)
	var scope struct{ ThreadID, TurnID, ItemID string }
	if err := json.Unmarshal(approval.Params, &scope); err != nil || scope.ThreadID != id || scope.TurnID != turn.Turn.ID || scope.ItemID == "" || len(approval.ID) == 0 {
		t.Fatal("approval not bound to native request/thread/turn/item", err)
	}
	f.mode.Store(0)
	if err := p.control.conn.WriteJSON(map[string]any{"id": approval.ID, "result": map[string]any{"decision": "decline"}}); err != nil {
		t.Fatal(err)
	}
	p.event("serverRequest/resolved", nil)
	p.event("item/completed", func(raw json.RawMessage) bool {
		return strings.Contains(string(raw), scope.ItemID) && strings.Contains(string(raw), `"declined"`)
	})
	p.event("turn/completed", nil)
	f.mode.Store(3)
	p.call("turn/start", map[string]any{"threadId": id, "collaborationMode": map[string]any{"mode": "plan", "settings": map[string]any{"model": "fixture", "reasoning_effort": nil, "developer_instructions": nil}}, "input": []any{map[string]any{"type": "text", "text": "isolated question probe"}}}, &turn)
	question := p.event("item/tool/requestUserInput", nil)
	if err := json.Unmarshal(question.Params, &scope); err != nil || scope.ThreadID != id || scope.TurnID != turn.Turn.ID || !strings.Contains(string(question.Params), "fixture_choice") {
		t.Fatal("question scope mismatch", err)
	}
	f.mode.Store(0)
	if err := p.control.conn.WriteJSON(map[string]any{"id": question.ID, "result": map[string]any{"answers": map[string]any{"fixture_choice": map[string]any{"answers": []string{"Continue"}}}}}); err != nil {
		t.Fatal(err)
	}
	p.event("serverRequest/resolved", nil)
	p.event("turn/completed", nil)
	t.Log("real CLI + local fixture: scoped native command approval, explicit decline, resolved receipt, plan-mode question and structured answer passed; no command accepted and external model calls=0")
}
