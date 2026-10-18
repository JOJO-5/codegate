package agent

import (
	"context"
	"encoding/json"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeV2NativeSmoke(t *testing.T) {
	executable := os.Getenv("CODEGATE_OPENCODE_V2_SMOKE")
	if executable == "" {
		t.Skip("native V2 smoke not requested")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()
	c := ResolvedCommand{Command: executable}
	env := os.Environ()
	v2, err := isOpenCodeV2(ctx, c, env, cwd)
	if err != nil || !v2 {
		t.Fatalf("V2 detection: %v %v", v2, err)
	}
	client, err := connectOpenCodeV2(ctx, c, env, cwd)
	if err != nil {
		t.Fatal(err)
	}
	binding := &protocol.ConversationBinding{}
	if err = client.prepare(ctx, binding); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := client.call(cleanupCtx, "DELETE", "/api/session/"+binding.NativeID, nil, nil); err != nil {
			t.Error(err)
		}
		// V2 caches filesystem services beyond the TUI lifetime. Dispose only
		// this fixture's location before Windows removes its temporary folder.
		if err := client.call(cleanupCtx, "DELETE", "/api/debug/location?"+url.Values{"location[directory]": {cwd}}.Encode(), nil, nil); err != nil {
			t.Error(err)
		}
	}()
	history, err := client.history(ctx, cwd, binding.NativeID)
	if err != nil || len(history) != 1 || history[0].ID != binding.NativeID {
		t.Fatalf("native history %d: %v", len(history), err)
	}
	if err = client.prepare(ctx, binding); err != nil {
		t.Fatalf("restore same native session: %v", err)
	}
	// Admit-only input exercises durable IDs and native inbox cancellation
	// without starting a model or consuming an account's allowance.
	var admitted struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = client.call(ctx, "POST", "/api/session/"+binding.NativeID+"/prompt", map[string]any{"id": "msg_codegate_smoke", "text": "fixture only", "resume": false}, &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.Data.ID != "msg_codegate_smoke" {
		t.Fatal("native admission ID changed")
	}
	if err = client.stopInput(ctx, binding.NativeID, "msg_codegate_smoke"); err != nil {
		t.Fatalf("cancel native pending input: %v", err)
	}
	var inbox struct {
		Data []json.RawMessage `json:"data"`
	}
	if err = client.call(ctx, "GET", "/api/session/"+binding.NativeID+"/inbox", nil, &inbox); err != nil || len(inbox.Data) != 0 {
		t.Fatalf("pending input not cancelled: %v", err)
	}
	var interrupted struct {
		Interrupted bool `json:"interrupted"`
	}
	if err = client.call(ctx, "POST", "/api/session/"+binding.NativeID+"/interrupt?resume=false", nil, &interrupted); err != nil || interrupted.Interrupted {
		t.Fatalf("idle interrupt: %+v %v", interrupted, err)
	}
	t.Log("native V2: explicit session identity, scoped history, restore and idle interrupt verified without submitting a model prompt")
	// Use CodeGate's actual terminal factory path and the prepared CLI args.
	a := &Agent{cfg: Config{StateDir: t.TempDir()}}
	newCommand := c
	newEnv, newBinding, newMarker, err := a.prepareConversation(&newCommand, BuildEnv(a.cfg, 100, 30), cwd, "opencode", nil)
	if err != nil || newMarker != "" || newBinding == nil || newBinding.NativeID == "" || nativeEnvValue(newEnv, "CODEGATE_OPENCODE_V2_URL") == "" {
		t.Fatalf("new V2 conversation and environment: %v", err)
	}
	defer client.call(context.Background(), "DELETE", "/api/session/"+newBinding.NativeID, nil, nil)
	prepared := c
	childEnv, bound, marker, err := a.prepareConversation(&prepared, env, cwd, "opencode", binding)
	if err != nil || marker != "" || bound.NativeID != binding.NativeID {
		t.Fatalf("V2 prepare: marker=%q err=%v", marker, err)
	}
	manager := session.NewManager(func(ctx context.Context, cfg terminal.StartConfig) (terminal.Terminal, error) {
		term := terminal.New()
		if err := term.Start(ctx, cfg); err != nil {
			return nil, err
		}
		return term, nil
	}, session.Config{})
	defer manager.CloseAll("native-smoke")
	sess, err := manager.Create(ctx, session.CreateRequest{Command: prepared.Command, Args: prepared.Args, Cwd: cwd, Env: childEnv, Cols: 100, Rows: 30, Recovery: bound})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
		t.Fatalf("V2 TUI exited unexpectedly: %+v", sess.Summary())
	case <-time.After(2 * time.Second):
	}
	if err = manager.Close(sess.ID, "native-smoke"); err != nil {
		t.Fatal(err)
	}
	var info struct {
		Version string `json:"version"`
	}
	if err = client.call(ctx, "GET", "/api/info", nil, &info); err != nil || !strings.HasPrefix(info.Version, "2.") {
		t.Fatalf("closing TUI stopped the shared service: %v", err)
	}
	t.Log("native V2 TUI starts in a real CodeGate terminal, restores its exact session and closes without stopping shared service")
}

func TestOpenCodeV2HistoryPaginationAndScope(t *testing.T) {
	cwd := t.TempDir()
	other := t.TempDir()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "private" {
			t.Error("missing local authentication")
		}
		pages++
		cursor := ""
		items := []openCodeV2Session{{ID: "ses_other", Location: openCodeV2Location{Directory: other}}}
		if r.URL.Query().Get("cursor") == "" {
			cursor = "page-two"
		} else {
			items = []openCodeV2Session{{ID: "ses_exact", Title: "real history", Location: openCodeV2Location{Directory: cwd}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": items, "cursor": map[string]any{"next": cursor}})
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, cwd)
	items, err := client.history(context.Background(), cwd, "")
	if err != nil || len(items) != 1 || items[0].ID != "ses_exact" || pages != 2 {
		t.Fatalf("history=%+v pages=%d err=%v", items, pages, err)
	}
}

func TestOpenCodeV2DirectoryAliasPreservesScope(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := openCodeV2Session{ID: "ses_exact", Location: openCodeV2Location{Directory: real}}
		if r.URL.Path == "/api/session" {
			json.NewEncoder(w).Encode(map[string]any{"data": []openCodeV2Session{s}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"data": s})
		}
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, alias)
	if _, err := client.session(t.Context(), "ses_exact"); err != nil {
		t.Fatal("same physical directory rejected", err)
	}
	if rows, err := client.history(t.Context(), alias, ""); err != nil || len(rows) != 1 {
		t.Fatal("aliased history missing", rows, err)
	}
	client.cwd = t.TempDir()
	if _, err := client.session(t.Context(), "ses_exact"); err == nil {
		t.Fatal("another physical directory accepted")
	}
}

func TestOpenCodeV2UsesServerGeneratedSessionIdentity(t *testing.T) {
	cwd := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if _, custom := body["id"]; custom {
				t.Error("custom session IDs can be rejected by the free-model gateway; use native generated identity")
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_nativeGenerated", "location": map[string]any{"directory": cwd}}})
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, cwd)
	binding := &protocol.ConversationBinding{}
	if err := client.prepare(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if binding.NativeID != "ses_nativeGenerated" {
		t.Fatal("native identity not retained")
	}
}

func testOpenCodeV2(t *testing.T, endpoint, cwd string) *openCodeV2Client {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return &openCodeV2Client{endpoint: u, password: "private", cwd: cwd, http: serverHTTPClient()}
}

func TestOpenCodeV2FreeTierFailureHasSafeRecovery(t *testing.T) {
	m := openCodeV2Message{}
	m.Error = &struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Status   int    `json:"status"`
		Response struct {
			Body string `json:"body"`
		} `json:"response"`
	}{Type: "provider.auth", Status: 403}
	m.Error.Response.Body = `{"type":"error","error":{"type":"FreeTierError","message":"SECRET_TOKEN"}}`
	code := openCodeV2ErrorCode(m)
	if code != "freeTierUnavailable" {
		t.Fatal("free gateway denial confused with account login", code)
	}
	detail := nativeFailureDetail(nativeTurn{Error: &struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}{Info: json.RawMessage(quoteJSON(code))}})
	if strings.Contains(detail, "SECRET") || !strings.Contains(detail, "免费") {
		t.Fatal("unsafe or unhelpful failure", detail)
	}
	m.Error.Status = 401
	m.Error.Message = "API key has expired"
	m.Error.Response.Body = ""
	if code := openCodeV2ErrorCode(m); code != "authenticationExpired" {
		t.Fatal("expired key classified as gateway eligibility", code)
	}
}

func TestOpenCodeV2StateAndInterruptOwnership(t *testing.T) {
	cwd := t.TempDir()
	active := true
	userID := "msg_owned"
	interrupts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_exact":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_exact", "location": map[string]any{"directory": cwd}, "time": map[string]any{"created": 1, "updated": 30, "idle": 30}, "outcome": "failed"}})
		case "/api/session/active":
			data := map[string]any{}
			if active {
				data["ses_exact"] = map[string]any{"type": "running"}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/api/session/ses_exact/message":
			if r.URL.Query().Get("type") == "user" {
				json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": userID, "type": "user", "time": map[string]any{"created": 10}}}})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "msg_assistant", "type": "assistant", "time": map[string]any{"created": 20}, "error": map[string]any{"type": "usage_limit_reached", "status": 429}}}})
			}
		case "/api/session/ses_exact/interrupt":
			interrupts++
			active = false
			json.NewEncoder(w).Encode(map[string]any{"interrupted": true})
		default:
			t.Errorf("unexpected route %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, cwd)
	_, turn, _, err := client.readTurn(context.Background(), "ses_exact")
	if err != nil || turn.ID != "msg_owned" || turn.Status != "inProgress" {
		t.Fatalf("active turn %+v: %v", turn, err)
	}
	active = false
	_, turn, _, err = client.readTurn(context.Background(), "ses_exact")
	if err != nil || !usageWall(turn) {
		t.Fatalf("structured quota wall %+v: %v", turn, err)
	}
	active = true
	if err = client.interrupt(context.Background(), "ses_exact", "msg_owned"); err != nil || active {
		t.Fatalf("native interrupt not confirmed: %v", err)
	}
	active = true
	userID = "msg_manual"
	if err = client.interrupt(context.Background(), "ses_exact", "msg_owned"); err == nil || interrupts != 1 {
		t.Fatalf("interrupted another turn: %v, calls=%d", err, interrupts)
	}
	client.cwd = filepath.Dir(cwd)
	if _, _, _, err = client.readTurn(context.Background(), "ses_exact"); err == nil {
		t.Fatal("accepted a session outside the authorized directory")
	}
}

func TestOpenCodeV2PromptUsesDurableIDAndLiteralBody(t *testing.T) {
	cwd := t.TempDir()
	const literal = "continue $(not-shell) & 中文"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.URL.Path != "/api/session/ses_exact/prompt" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["id"] != "msg_owned" || body["text"] != literal {
			t.Errorf("literal input changed: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": body["id"]}})
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, cwd)
	if err := client.prompt(context.Background(), "ses_exact", "msg_owned", literal); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeV2EnvironmentKeepsServiceCredentialPrivate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/session/ses_owned/environment" {
			t.Errorf("unexpected environment request %s %s", r.Method, r.URL)
		}
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Variables) != 2 || body.Variables["PATH"] != "approved-path" || body.Variables["PROJECT_VALUE"] != "a=b" {
			t.Errorf("unexpected environment keys or values: %v", body.Variables)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	client := testOpenCodeV2(t, server.URL, t.TempDir())
	if err := client.environment(t.Context(), "ses_owned", []string{"PATH=approved-path", "PROJECT_VALUE=a=b", "OPENCODE_PASSWORD=private", "opencode_server_password=private", "CODEGATE_OPENCODE_V2_URL=http://local", "CODEGATE_CONVERSATION_MARKER=private-marker"}); err != nil {
		t.Fatal(err)
	}
}
