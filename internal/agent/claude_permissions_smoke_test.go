package agent

import (
	"bufio"
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

func TestClaudeNativePermissionModesSmoke(t *testing.T) {
	binary := os.Getenv("CODEGATE_CLAUDE_PERMISSION_SMOKE")
	if binary == "" {
		t.Skip("set CODEGATE_CLAUDE_PERMISSION_SMOKE to installed Claude executable")
	}
	for _, mode := range []string{"manual", "plan", "acceptEdits", "dontAsk", "bypassPermissions"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{}`)
					return
				}
				posts.Add(1)
				io.Copy(io.Discard, io.LimitReader(r.Body, 4<<20))
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(kind string, data map[string]any) {
					data["type"] = kind
					b, _ := json.Marshal(data)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
					w.(http.Flusher).Flush()
				}
				emit("message_start", map[string]any{"message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
				emit("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				emit("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": "CodeGate isolated fixture"}})
				emit("content_block_stop", map[string]any{"index": 0})
				emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
				emit("message_stop", map[string]any{})
			}))
			defer provider.Close()
			root, home := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--print", "--verbose", "--output-format", "stream-json", "--safe-mode", "--setting-sources", "", "--no-chrome", "--model", "fixture", "--permission-mode", mode, "CodeGate isolated fixture prompt")
			cmd.Dir = root
			cmd.Env = BuildEnv(Config{Env: map[string]string{"CLAUDE_CONFIG_DIR": home, "ANTHROPIC_API_KEY": "local-fixture-no-credentials", "ANTHROPIC_BASE_URL": provider.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1", "DISABLE_TELEMETRY": "1"}}, 100, 30)
			processutil.Background(cmd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("isolated Claude startup failed: %v", err)
			}
			found := false
			scanner := bufio.NewScanner(strings.NewReader(string(out)))
			scanner.Buffer(make([]byte, 4096), 4<<20)
			for scanner.Scan() {
				var event struct{ Type, Subtype, PermissionMode string }
				if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "system" && event.Subtype == "init" {
					want := mode
					if mode == "manual" {
						want = "default"
					} // Native stream uses the historical canonical name.
					if event.PermissionMode != want {
						t.Fatalf("native mode differs: %q", event.PermissionMode)
					}
					found = true
				}
			}
			if !found || posts.Load() == 0 {
				t.Fatal("native init receipt or fixture model response missing")
			}
			t.Log("native init confirmed permission mode; local fake provider only, no tools or real model")
		})
	}
}
