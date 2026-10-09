package agent

import (
	"context"
	"encoding/json"
	"io"
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
		case "https://opencode.ai/zen/go/v1/usage":
			if r.Header.Get("Authorization") != "Bearer private-go-token" {
				t.Fatal("incorrect Go credentials")
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
