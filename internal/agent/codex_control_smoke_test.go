package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

// Explicitly opted in: reads native control state only, never starts or resumes
// a model turn and never emits conversation contents, identities or credentials.
func TestCodexNativeControlSmoke(t *testing.T) {
	if os.Getenv("CODEGATE_CODEX_CONTROL_SMOKE") != "1" {
		t.Skip("local daemon smoke not requested")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	control, err := openCodexControl(ctx, codexHome(BuildEnv(Config{}, 80, 24)))
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	var loaded struct {
		Data []string `json:"data"`
	}
	if err := control.call("thread/loaded/list", map[string]any{}, &loaded); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, id := range loaded.Data {
		var result struct {
			Thread nativeThread `json:"thread"`
		}
		if err := control.call("thread/read", map[string]any{"threadId": id, "includeTurns": false}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Thread.Status.Type == "active" {
			active++
		}
	}
	t.Logf("native handshake and read-only RPC succeeded; loaded=%d active=%d", len(loaded.Data), active)
}
