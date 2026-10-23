package agent

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
)

// Runs a real installed CLI against isolated state and a local fake provider.
// No production credentials, conversations or model endpoints are used.
func TestCodexDaemonTerminalSmoke(t *testing.T) {
	binary := os.Getenv("CODEGATE_CODEX_DAEMON_SMOKE")
	if binary == "" {
		t.Skip("isolated real Codex daemon smoke not requested")
	}
	home, err := os.MkdirTemp("", "cg-cdx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	var generations atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			generations.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer provider.Close()
	config := fmt.Sprintf("model = \"fixture\"\nmodel_provider = \"fixture\"\n[model_providers.fixture]\nname = \"Local isolated test\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = \"CODEGATE_LOCAL_FAKE_KEY\"\n", provider.URL+"/v1")
	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Env: map[string]string{"CODEX_HOME": home, "CODEGATE_LOCAL_FAKE_KEY": "local-dummy-no-credentials"}}
	env := BuildEnv(cfg, 100, 30)
	root := t.TempDir()
	c := ResolvedCommand{Command: binary}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(ctx, binary, "app-server", "daemon", "stop")
		stop.Env = env
		processutil.Background(stop)
		if err := stop.Run(); err != nil {
			t.Error("isolated daemon cleanup", err)
		}
	})
	cfg.ServerURL = "https://localhost.invalid"
	cfg.StateDir = t.TempDir()
	cfg.AllowedRoots = []string{root}
	cfg.AllowedCommands = []CommandSpec{{ID: "codex", Command: binary, Kind: KindTUI}}
	a, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.mgr.CloseAll("isolated smoke complete") })
	if err = a.prepareCodexDaemon(&c, env, root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	control, err := openCodexControl(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	var started struct {
		Thread nativeThread `json:"thread"`
	}
	if err = control.call("thread/start", map[string]any{"cwd": root, "model": "fixture", "modelProvider": "fixture", "approvalPolicy": "never", "sandbox": "read-only"}, &started); err != nil {
		t.Fatal(err)
	}
	if started.Thread.ID == "" {
		t.Fatal("empty thread identity")
	}
	// Persist a real native history entry through the installed CLI without a
	// model turn. This text belongs only to the isolated smoke directory.
	var injected json.RawMessage
	if err = control.call("thread/inject_items", map[string]any{"threadId": started.Thread.ID, "items": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "CodeGate isolated recovery marker"}}},
	}}, &injected); err != nil {
		t.Fatal("persist isolated native history", err)
	}
	// Starting a second terminal must preserve the same running service/thread.
	second := ResolvedCommand{Command: binary}
	if err = a.prepareCodexDaemon(&second, env, root); err != nil {
		t.Fatal(err)
	}
	var before struct {
		Thread nativeThread `json:"thread"`
	}
	if err = control.call("thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": false}, &before); err != nil || before.Thread.Status.Type == "notLoaded" {
		t.Fatal("service was replaced", err)
	}
	sess, err := a.createSession(protocol.SessionCreatePayload{CommandID: "codex", Cwd: root, Cols: 100, Rows: 30, Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: started.Thread.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// An idle TUI should stay alive without any prompt/model invocation.
	select {
	case <-sess.Done():
		t.Fatal("remote terminal exited before user input")
	case <-time.After(3 * time.Second):
	}
	if generations.Load() != 0 {
		t.Fatal("opening an idle terminal invoked a model")
	}
	var raw json.RawMessage
	if err = control.call("thread/loaded/list", map[string]any{}, &raw); err != nil {
		t.Fatal(err)
	}
	source := sess.Summary()
	if err = a.mgr.Close(sess.ID, "isolated terminal closed"); err != nil {
		t.Fatal(err)
	}
	source.Status = "terminated"
	if err = control.call("thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": false}, &before); err != nil || before.Thread.Status.Type == "notLoaded" {
		t.Fatal("closing terminal stopped shared service", err)
	}
	// Exercise the production recovery handler, replacing only the server wire.
	// History lookup, white-list checks, daemon, CLI resume and PTY are real.
	wire := &Conn{send: make(chan outbound, 64), closed: make(chan struct{})}
	a.setConn(wire)
	request := func(source protocol.SessionSummary) *protocol.Envelope {
		t.Helper()
		req, e := protocol.NewRequest(uuid.NewString(), protocol.TypeConversationRestore, source.SessionID,
			protocol.ConversationRequest{SessionID: source.SessionID, Source: source, Cols: 100, Rows: 30})
		if e != nil {
			t.Fatal(e)
		}
		a.onConversationRequest(req)
		for {
			select {
			case msg := <-wire.send:
				reply, e := protocol.Decode(msg.data)
				if e != nil {
					t.Fatal(e)
				}
				if reply.ReplyTo == req.RequestID {
					return reply
				}
			case <-ctx.Done():
				t.Fatal("recovery reply timed out")
			}
		}
	}
	missing := source
	missing.Recovery = &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}
	if request(missing).Type != protocol.TypeError || len(a.mgr.Snapshot()) != 0 {
		t.Fatal("missing native history must fail without creating a terminal")
	}
	reply := request(source)
	if reply.Type != protocol.TypeSessionCreated {
		t.Fatalf("real native recovery failed: %s", reply.Payload)
	}
	restored, err := protocol.DecodePayload[protocol.SessionCreatedPayload](reply)
	if err != nil || restored.Session.Recovery == nil || restored.Session.Recovery.NativeID != started.Thread.ID || restored.Session.SessionID == source.SessionID {
		t.Fatal("recovery lost original native identity", err)
	}
	resumed, ok := a.mgr.Get(parseUUID(restored.Session.SessionID))
	if !ok {
		t.Fatal("restored PTY missing")
	}
	select {
	case <-resumed.Done():
		t.Fatal("real CLI resume exited before user input")
	case <-time.After(3 * time.Second):
	}
	again, err := protocol.DecodePayload[protocol.SessionCreatedPayload](request(source))
	if err != nil || again.Session.SessionID != restored.Session.SessionID || len(a.mgr.Snapshot()) != 1 {
		t.Fatal("repeated recovery created another process", err)
	}
	if generations.Load() != 0 {
		t.Fatal("native recovery invoked a model")
	}
	t.Log("isolated real CLI startup, saved history, close, missing-history failure, original conversation recovery and duplicate reuse passed; model calls=0")
}
