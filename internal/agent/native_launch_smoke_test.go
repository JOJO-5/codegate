package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

func TestCodexNativeTerminalLaunchModesSmoke(t *testing.T) {
	for _, tc := range []struct{ id, sandbox, approval, reviewer string }{
		{"codex.read-only", "readOnly", "on-request", "user"},
		{"codex.workspace", "workspaceWrite", "on-request", "user"},
		{"codex.auto", "workspaceWrite", "on-request", "auto_review"},
		{"codex.full-access", "dangerFullAccess", "never", "user"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			f := newCodexChatFixture(t)
			f.a.cfg.AllowedCommands[0].Args = nil
			p := f.connect(t)
			sess, err := f.a.createSession(protocol.SessionCreatePayload{CommandID: "codex", Cwd: f.root, Cols: 100, Rows: 30, LaunchMode: tc.id})
			if err != nil {
				t.Fatal(err)
			}
			var loaded struct {
				Data []string `json:"data"`
			}
			deadline := time.Now().Add(8 * time.Second)
			for len(loaded.Data) == 0 && time.Now().Before(deadline) {
				select {
				case <-sess.Done():
					t.Fatal("native terminal exited during startup")
				default:
				}
				p.call("thread/loaded/list", map[string]any{}, &loaded)
				if len(loaded.Data) == 0 {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if len(loaded.Data) != 1 {
				t.Fatal("native terminal did not expose exactly one owned thread")
			}
			var state codexProbeThread
			var saved json.RawMessage
			p.call("thread/name/set",map[string]any{"threadId":loaded.Data[0],"name":"CodeGate isolated native mode"},&saved)
			p.call("thread/resume", map[string]any{"threadId": loaded.Data[0], "excludeTurns": true}, &state)
			if state.ApprovalPolicy != tc.approval || state.Reviewer != tc.reviewer || !strings.Contains(string(state.Sandbox), `"`+tc.sandbox+`"`) {
				t.Fatalf("native TUI mode differs: approval=%s reviewer=%s sandbox=%s", state.ApprovalPolicy, state.Reviewer, state.Sandbox)
			}
			if f.posts.Load() != 0 {
				t.Fatal("opening mode-selected idle TUI invoked a model")
			}
			t.Log("actual native TUI startup confirmed requested CLI mode; no model turn, synthetic home/project only")
		})
	}
}
