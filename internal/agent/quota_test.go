package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type quotaTransport func(*http.Request) (*http.Response, error)

func (f quotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestQuotaNativeCredentialsAndSanitization(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "opencode"), 0700)
	os.WriteFile(filepath.Join(root, "opencode", "auth.json"), []byte(`{"opencode-go":{"type":"api","key":"private-go-token"}}`), 0600)
	os.WriteFile(filepath.Join(root, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"private-claude-token"}}`), 0600)
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case "https://opencode.ai/inference/go/v1/usage":
			if r.Header.Get("Authorization") != "Bearer private-go-token" {
				t.Fatal("incorrect Go credentials")
			}
			if !strings.HasPrefix(r.UserAgent(), "codegate-agent/") {
				t.Fatal("Go usage requires an identifiable client")
			}
			body = `{"usage":{"rolling":{"percent":1,"resetsAt":"2026-10-10T00:00:00Z"},"weekly":{"percent":100}}}`
		case "https://api.anthropic.com/api/oauth/usage":
			if r.Header.Get("Authorization") != "Bearer private-claude-token" || r.Header.Get("anthropic-beta") == "" {
				t.Fatal("incorrect Claude credentials")
			}
			body = `{"five_hour":{"utilization":0,"resets_at":"2026-10-10T00:00:00Z"},"seven_day":{"utilization":65}}`
		default:
			t.Fatal("unexpected endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	env := []string{"XDG_DATA_HOME=" + root, "CLAUDE_CONFIG_DIR=" + root}
	goWindows, message := openCodeQuota(context.Background(), env, client)
	if message != "" || len(goWindows) != 2 || goWindows[0].UsedPercent != 1 || goWindows[1].ResetsAt != 0 {
		t.Fatalf("Go parsing: %+v %s", goWindows, message)
	}
	claudeWindows, message := claudeQuota(context.Background(), env, client)
	if message != "" || len(claudeWindows) != 2 || claudeWindows[0].UsedPercent != 0 {
		t.Fatalf("Claude parsing: %+v %s", claudeWindows, message)
	}
	encoded, _ := json.Marshal(claudeWindows)
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("credential leak")
	}
}

func TestOpenCodeQuotaCredentialFailuresHaveActionableRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"expired key", `{"error":{"code":"api_key_expired","message":"private server details"}}`, "已过期", 401},
		{"other auth failure", `{"error":{"code":"api_key_invalid","message":"private server details"}}`, "API Key", 401},
		{"blocked query", `{"cloudflare_error":true,"detail":"private server details"}`, "无法确认订阅状态", 403},
		{"non JSON auth failure", `<html>private server details</html>`, "API Key", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			windows, message := openCodeQuota(context.Background(), []string{"OPENCODE_API_KEY=private-go-token", "XDG_DATA_HOME=" + t.TempDir()}, client)
			if len(windows) != 0 || !strings.Contains(message, tc.want) || strings.Contains(message, "重新登录") || strings.Contains(message, "未订阅") || strings.Contains(message, "private") {
				t.Fatalf("unexpected failure state: %+v %s", windows, message)
			}
		})
	}
}

func TestOpenCodeQuotaUnavailableThenRecoversAfterKeyReplacement(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "opencode"), 0700)
	path := filepath.Join(root, "opencode", "auth.json")
	os.WriteFile(path, []byte(`{"opencode-go":{"type":"api","key":"old-key"}}`), 0600)
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 401, `{"error":{"code":"api_key_expired"}}`
		if r.Header.Get("Authorization") == "Bearer new-key" {
			status, body = 200, `{"usage":{"rolling":{"percent":0},"weekly":{"percent":36},"monthly":{"percent":100}}}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	env := []string{"XDG_DATA_HOME=" + root}
	if windows, message := openCodeQuota(context.Background(), env, client); len(windows) != 0 || !strings.Contains(message, "已过期") {
		t.Fatal("expired credential must not produce numeric quota")
	}
	os.WriteFile(path, []byte(`{"opencode-go":{"type":"api","key":"new-key"}}`), 0600)
	windows, message := openCodeQuota(context.Background(), env, client)
	if message != "" || len(windows) != 3 || windows[0].UsedPercent != 0 || windows[2].UsedPercent != 100 {
		t.Fatalf("replacement credential not read on refresh: %+v %s", windows, message)
	}
}

func TestOpenCodeQuotaUsesV2CredentialWithoutFallingBackToStaleKey(t *testing.T) {
	for _, tc := range []struct {
		name, values, wantToken, wantMessage string
	}{
		{"new key", `('opencode-go',1,'{"type":"key","key":"current-key"}')`, "current-key", ""},
		{"single migrated key", `('opencode-go',0,'{"type":"api","key":"migrated-key"}')`, "migrated-key", ""},
		{"active selection", `('opencode-go',0,'{"type":"key","key":"old-key"}'),('opencode-go',1,'{"type":"key","key":"selected-key"}')`, "selected-key", ""},
		{"removed Go credential", `('other-provider',1,'{"type":"key","key":"other-key"}')`, "", "未配置"},
		{"ambiguous selection", `('opencode-go',0,'{"type":"key","key":"first-key"}'),('opencode-go',0,'{"type":"key","key":"second-key"}')`, "", "多个"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "opencode")
			os.MkdirAll(dir, 0700)
			os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"opencode-go":{"type":"api","key":"stale-key"}}`), 0600)
			db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`CREATE TABLE credential (integration_id TEXT, active INTEGER, value TEXT); INSERT INTO credential VALUES ` + tc.values); err != nil {
				t.Fatal(err)
			}
			db.Close()
			calls := 0
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.wantToken == "" || r.Header.Get("Authorization") != "Bearer "+tc.wantToken {
					t.Fatal("queried incorrect local credential")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"percent":4}}}`)), Header: make(http.Header)}, nil
			})}
			windows, message := openCodeQuota(context.Background(), []string{"XDG_DATA_HOME=" + root}, client)
			if tc.wantMessage != "" {
				if calls != 0 || len(windows) != 0 || !strings.Contains(message, tc.wantMessage) {
					t.Fatalf("unexpected unavailable state: %s", message)
				}
			} else if message != "" || calls != 1 || len(windows) != 1 {
				t.Fatalf("V2 query failed: %s", message)
			}
		})
	}
}
func TestQuotaRejectsMissingAndInvalidValues(t *testing.T) {
	for _, v := range []float64{-1, 101} {
		if _, ok := quotaWindow("test", &v, 0); ok {
			t.Fatal("accepted invalid quota")
		}
	}
	if _, ok := quotaWindow("test", nil, 0); ok {
		t.Fatal("missing quota became 100% remaining")
	}
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private server error with token"))}, nil
	})}
	var out any
	message := quotaGet(context.Background(), client, "https://opencode.ai/zen/go/v1/usage", "secret", false, &out)
	if strings.Contains(message, "token") || !strings.Contains(message, "登录") {
		t.Fatal(message)
	}
}

// Helper executable speaks the real app-server handshake, never runs an LLM.
func TestQuotaRPCProcess(t *testing.T) {
	if os.Getenv("CODEGATE_QUOTA_HELPER") == "1" {
		decoder := json.NewDecoder(os.Stdin)
		var req map[string]any
		decoder.Decode(&req)
		if req["method"] != "initialize" {
			os.Exit(2)
		}
		io.WriteString(os.Stdout, "{\"id\":1,\"result\":{}}\n")
		decoder.Decode(&req)
		if req["method"] != "initialized" {
			os.Exit(3)
		}
		decoder.Decode(&req)
		if req["method"] != "account/rateLimits/read" {
			os.Exit(4)
		}
		io.WriteString(os.Stdout, `{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1800000000},"secondary":{"usedPercent":80,"windowDurationMins":10080}}}}`+"\n")
		os.Exit(0)
	}
	// Copy a dedicated test binary and select only this test via environment flags.
	if os.PathSeparator == '\\' {
		t.Skip("helper executable flags differ on Windows; native cross-build covered by CI")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := filepath.Join(root, "codex")
	text := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run '^TestQuotaRPCProcess$'\n"
	if err := os.WriteFile(script, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	windows, message := codexQuota(ctx, ResolvedCommand{Command: script}, append(os.Environ(), "CODEGATE_QUOTA_HELPER=1"))
	if message != "" || len(windows) != 2 || windows[0].Label != "5 小时窗口" || windows[1].Label != "7 天窗口" {
		t.Fatalf("RPC: %+v %s", windows, message)
	}
}
func TestQuotaRealCodexLoggedOut(t *testing.T) {
	binary := os.Getenv("CODEGATE_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("optional installed native CLI check")
	}
	if _, err := exec.LookPath(binary); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	windows, message := codexQuota(ctx, ResolvedCommand{Command: binary}, append(BuildEnv(Config{}, 80, 24), "CODEX_HOME="+t.TempDir()))
	if len(windows) != 0 || !strings.Contains(message, "登录") {
		t.Fatalf("logged out native CLI: %+v %s", windows, message)
	}
}

func TestSessionQuotaProvider(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    string
	}{
		{"/usr/bin/codex", nil, "codex"}, {`C:\tools\codex.exe`, nil, "codex"},
		{`C:\Windows\cmd.exe`, []string{"/d", "/c", `C:\npm\claude.cmd`}, "claude"},
		{"opencode", nil, "opencode"}, {"bash", []string{"codex"}, ""}, {"dsh", nil, ""},
	} {
		if got := quotaProvider(tc.command, tc.args); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.command, got, tc.want)
		}
	}
}

func TestQuotaReadOnlyReturnsSessionCLIAndKeepsIndependentCaches(t *testing.T) {
	mgr := session.NewManager(func(context.Context, terminal.StartConfig) (terminal.Terminal, error) {
		return &filesTestTerminal{done: make(chan struct{})}, nil
	}, session.Config{})
	defer mgr.CloseAll("test")
	conn := &Conn{send: make(chan outbound, 4), closed: make(chan struct{})}
	a := &Agent{mgr: mgr, log: slog.Default(), quotaCache: map[string]quotaCacheEntry{}}
	a.setConn(conn)
	for _, provider := range []string{"codex", "claude", "opencode"} {
		a.quotaCache[provider] = quotaCacheEntry{result: protocol.ProviderQuota{Provider: provider, CheckedAt: 123}, checked: time.Now()}
	}
	for _, command := range []string{"codex", "claude", "opencode", "bash"} {
		sess, err := mgr.Create(context.Background(), session.CreateRequest{DeviceID: uuid.New(), UserID: uuid.New(), Cwd: t.TempDir(), Command: command, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeQuotaRead, sess.ID.String(), map[string]string{"session_id": sess.ID.String(), "provider": "other"})
		a.onQuotaRead(req)
		env, err := protocol.Decode((<-conn.send).data)
		if err != nil {
			t.Fatal(err)
		}
		result, err := protocol.DecodePayload[protocol.QuotaResult](env)
		if err != nil {
			t.Fatal(err)
		}
		if command == "bash" {
			if len(result.Providers) != 0 {
				t.Fatal("shell received quotas")
			}
		} else if len(result.Providers) != 1 || result.Providers[0].Provider != command || result.Providers[0].CheckedAt != 123 {
			t.Fatalf("wrong provider/cache for %s: %+v", command, result)
		}
	}
	if len(a.quotaCache) != 3 {
		t.Fatal("cross-provider cache changed")
	}
}

func TestQuotaPendingProviderDoesNotBlockCachedOtherProvider(t *testing.T) {
	pending := &quotaFlight{done: make(chan struct{})}
	a := &Agent{quotaCache: map[string]quotaCacheEntry{"claude": {result: protocol.ProviderQuota{Provider: "claude", CheckedAt: 123}, checked: time.Now()}}, quotaFlights: map[string]*quotaFlight{"codex": pending}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, err := a.readProviderQuota(ctx, "claude")
	if err != nil || !result.Cached || len(result.Providers) != 1 || result.Providers[0].Provider != "claude" || result.NextRefreshAt == 0 {
		t.Fatal("cached provider blocked", err, result)
	}
	if _, err := a.readProviderQuota(ctx, "codex"); err != context.DeadlineExceeded {
		t.Fatal("pending query ignored deadline", err)
	}
	pending.result = protocol.ProviderQuota{Provider: "codex", CheckedAt: 456}
	close(pending.done)
	result, err = a.readProviderQuota(context.Background(), "codex")
	if err != nil || result.Cached || result.Providers[0].CheckedAt != 456 {
		t.Fatal("shared completion lost", err)
	}
}

func TestQuotaStalledProcessHonorsDeadline(t *testing.T) {
	if os.Getenv("CODEGATE_QUOTA_STALL") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if os.PathSeparator == '\\' {
		t.Skip("shell helper; Windows private Job cleanup covered by processutil tests")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(binary, "'", "'\\''")+"' -test.run '^TestQuotaStalledProcessHonorsDeadline$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	windows, message := codexQuota(ctx, ResolvedCommand{Command: script}, append(os.Environ(), "CODEGATE_QUOTA_STALL=1"))
	if len(windows) != 0 || message == "" || time.Since(start) > 2*time.Second {
		t.Fatal("stalled app-server did not stop", message, time.Since(start))
	}
}
