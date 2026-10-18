package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

// Opt in explicitly: ordinary CI never calls a hosted model. Only an exact
// currently zero-priced model is accepted; there is no provider/model fallback.
func TestOpenCodeV2FreeModelExecution(t *testing.T) {
	executable, selection := os.Getenv("CODEGATE_OPENCODE_V2_SMOKE"), os.Getenv("CODEGATE_OPENCODE_V2_FREE_MODEL")
	if executable == "" || selection == "" {
		t.Skip("native free-model execution not requested")
	}
	provider, model, ok := strings.Cut(selection, "/")
	if !ok || provider != "opencode" || (!strings.HasSuffix(model, "-free") && model != "big-pickle") {
		t.Fatal("select an explicit OpenCode free model")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cwd := t.TempDir()
	client, err := connectOpenCodeV2(ctx, ResolvedCommand{Command: executable}, BuildEnv(Config{}, 80, 24), cwd)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Status     string `json:"status"`
			Cost       []struct {
				Input  *float64 `json:"input"`
				Output *float64 `json:"output"`
				Cache  struct {
					Read  *float64 `json:"read"`
					Write *float64 `json:"write"`
				} `json:"cache"`
			} `json:"cost"`
		} `json:"data"`
	}
	if err = client.call(ctx, "GET", "/api/model?"+url.Values{"location[directory]": {cwd}}.Encode(), nil, &catalog); err != nil {
		t.Fatal(err)
	}
	free := false
	for _, m := range catalog.Data {
		if m.ProviderID == provider && m.ID == model && m.Status == "active" && len(m.Cost) > 0 {
			free = true
			for _, tier := range m.Cost {
				for _, price := range []*float64{tier.Input, tier.Output, tier.Cache.Read, tier.Cache.Write} {
					if price == nil || *price != 0 {
						free = false
					}
				}
			}
		}
	}
	if !free {
		t.Fatal("selected model has no complete current zero-price metadata; no model request sent")
	}
	ids := []string{}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, id := range ids {
			if err := client.call(cleanup, "POST", "/api/session/"+id+"/interrupt?resume=false", nil, nil); err != nil {
				t.Error(err)
			}
			if err := client.call(cleanup, "DELETE", "/api/session/"+id, nil, nil); err != nil {
				t.Error(err)
			}
		}
		if err := client.call(cleanup, "DELETE", "/api/debug/location?"+url.Values{"location[directory]": {cwd}}.Encode(), nil, nil); err != nil {
			t.Error(err)
		}
	}()
	create := func() string {
		t.Helper()
		binding := &protocol.ConversationBinding{}
		if err := client.prepare(ctx, binding); err != nil {
			t.Fatal(err)
		}
		id := binding.NativeID
		ids = append(ids, id)
		if err := client.call(ctx, "POST", "/api/session/"+id+"/model", map[string]any{"model": map[string]string{"providerID": provider, "id": model}}, nil); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Data struct {
				ID    string `json:"id"`
				Model struct {
					ID         string `json:"id"`
					ProviderID string `json:"providerID"`
				} `json:"model"`
			} `json:"data"`
		}
		err := client.call(ctx, "GET", "/api/session/"+id, nil, &response)
		if err != nil {
			t.Fatal(err)
		}
		if response.Data.ID != id || response.Data.Model.ID != model || response.Data.Model.ProviderID != provider {
			t.Fatal("native model selection changed")
		}
		return id
	}
	completed := create()
	if os.Getenv("CODEGATE_OPENCODE_V2_ENV_SMOKE") == "1" {
		if err := client.environment(ctx, completed, BuildEnv(Config{}, 80, 24)); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("CODEGATE_OPENCODE_V2_PERMISSION_SMOKE") == "1" {
		if err := client.call(ctx, "PATCH", "/api/session/"+completed, map[string]any{"permissions": workflowOpenCodePermissions(true)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	message := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err = client.prompt(ctx, completed, message, "Reply with exactly CODEGATE_FREE_OK. Do not use tools, read files, or execute commands."); err != nil {
		t.Fatal(err)
	}
	for {
		_, turn, text, err := client.readTurn(ctx, completed)
		if err == nil && turn.ID == message && turn.Status != "inProgress" {
			if turn.Status != "completed" || strings.TrimSpace(text) != "CODEGATE_FREE_OK" {
				var latest struct {
					Data []struct {
						Error json.RawMessage `json:"error"`
					} `json:"data"`
				}
				if e := client.call(ctx, "GET", "/api/session/"+completed+"/message?type=assistant&limit=1&order=desc", nil, &latest); e == nil && len(latest.Data) > 0 {
					var native struct {
						Type     string `json:"type"`
						Status   int    `json:"status"`
						Message  string `json:"message"`
						Response struct {
							Body string `json:"body"`
						} `json:"response"`
					}
					_ = json.Unmarshal(latest.Data[0].Error, &native)
					low := strings.ToLower(string(latest.Data[0].Error))
					flags := []string{}
					for _, known := range []string{"freetiererror", "free tier", "unauthorized", "forbidden", "authentication", "api key", "invalid api", "invalid session", "expired", "login", "sign in", "quota"} {
						if strings.Contains(low, known) {
							flags = append(flags, known)
						}
					}
					t.Logf("safe native diagnostic: status=%d message_bytes=%d response_bytes=%d known_error_flags=%v", native.Status, len(native.Message), len(native.Response.Body), flags)
				}
				t.Fatalf("free model outcome=%s output=%q error=%+v", turn.Status, text, turn.Error)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("free model did not complete: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Logf("%s completed a real native prompt with exact expected output", selection)
	running := create()
	own := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err = client.prompt(ctx, running, own, "Write a detailed 2000-word explanation of merge sort. Do not use tools, read files, or execute commands."); err != nil {
		t.Fatal(err)
	}
	for {
		_, turn, _, err := client.readTurn(ctx, running)
		if err == nil && turn.ID == own && turn.Status == "inProgress" {
			break
		}
		if err == nil && turn.ID == own && turn.Status != "inProgress" {
			t.Fatalf("second prompt finished before cancellation: %s", turn.Status)
		}
		select {
		case <-ctx.Done():
			t.Fatal("native execution did not start")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = client.interrupt(ctx, running, own); err != nil {
		t.Fatal(err)
	}
	_, turn, _, err := client.readTurn(ctx, running)
	if err != nil || turn.ID != own || turn.Status != "interrupted" {
		t.Fatalf("native cancellation outcome=%s err=%v", turn.Status, err)
	}
	// The completed sibling session must retain its result after cancellation.
	_, sibling, text, err := client.readTurn(ctx, completed)
	if err != nil || sibling.Status != "completed" || strings.TrimSpace(text) != "CODEGATE_FREE_OK" {
		t.Fatalf("cancellation affected sibling session: %v", err)
	}
	for _, id := range ids {
		var result struct {
			Data struct {
				Cost *float64 `json:"cost"`
			} `json:"data"`
		}
		if err = client.call(ctx, "GET", "/api/session/"+id, nil, &result); err != nil || result.Data.Cost == nil || *result.Data.Cost != 0 {
			t.Fatalf("native session cost is not confirmed zero: %v", err)
		}
	}
	t.Log("running native execution cancelled; sibling output intact; both native session costs are zero")
}
