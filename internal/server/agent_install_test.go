package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestFirstInstallTicketIsScopedAndSingleUse(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.BaseURL = "https://codegate.example.test"
	dir := t.TempDir()
	e.srv.cfg.AgentUpdatesDir = dir
	payload := []byte("test agent")
	for _, arch := range []string{"amd64", "arm64"} {
		p := filepath.Join(dir, "linux", arch)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "version.txt"), []byte("v0.1.0"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "codegate-agent"), payload, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	access := e.signup(t, "install@example.com", "TestPassword123!")
	issued := e.post(t, "/api/v1/agent-install-ticket", access, map[string]string{"os": "linux"})
	if issued.Status != http.StatusOK {
		t.Fatalf("issue: %d %s", issued.Status, issued.Body)
	}
	command := issued.Str(t, "command")
	key := regexp.MustCompile(`ticket=([A-Za-z0-9_-]+)`).FindStringSubmatch(command)
	if len(key) != 2 {
		t.Fatalf("ticket missing from %q", command)
	}
	script := e.get(t, "/api/v1/agent-install/linux/script?ticket="+key[1], "")
	if script.Status != http.StatusOK {
		t.Fatalf("script: %d %s", script.Status, script.Body)
	}
	sum := sha256.Sum256(payload)
	if !bytes.Contains(script.Body, []byte(hex.EncodeToString(sum[:]))) {
		t.Fatal("script lacks checksum")
	}
	getBinary := func(platform string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, e.http.URL+"/api/v1/agent-install/"+platform+"/amd64/binary", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-CodeGate-Install-Ticket", key[1])
		resp, err := e.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	if status, _ := getBinary("windows"); status != http.StatusUnauthorized {
		t.Fatalf("cross platform %d", status)
	}
	if status, data := getBinary("linux"); status != http.StatusOK || !bytes.Equal(data, payload) {
		t.Fatalf("download: %d %q", status, data)
	}
	if status, _ := getBinary("linux"); status != http.StatusUnauthorized {
		t.Fatalf("ticket replay: %d", status)
	}
	issued = e.post(t, "/api/v1/agent-install-ticket", access, map[string]string{"os": "linux"})
	var second struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(issued.Body, &second); err != nil {
		t.Fatal(err)
	}
	key = regexp.MustCompile(`ticket=([A-Za-z0-9_-]+)`).FindStringSubmatch(second.Command)
	e.clock.Advance(installTicketTTL + time.Second)
	if got := e.get(t, "/api/v1/agent-install/linux/script?ticket="+key[1], ""); got.Status != http.StatusUnauthorized {
		t.Fatalf("expired ticket %d", got.Status)
	}
}

func TestWindowsInstallerStagesNewSupervisorForExistingInstall(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.BaseURL = "https://codegate.example.test"
	dir := t.TempDir()
	e.srv.cfg.AgentUpdatesDir = dir
	for _, arch := range []string{"amd64", "arm64"} {
		p := filepath.Join(dir, "windows", arch)
		os.MkdirAll(p, 0700)
		os.WriteFile(filepath.Join(p, "version.txt"), []byte("v0.1.26"), 0600)
		os.WriteFile(filepath.Join(p, "codegate-agent.exe"), []byte("verified bootstrap"), 0700)
	}
	access := e.signup(t, "windows-install@example.com", "TestPassword123!")
	issued := e.post(t, "/api/v1/agent-install-ticket", access, map[string]string{"os": "windows"})
	if issued.Status != 200 {
		t.Fatal(issued.Status)
	}
	key := regexp.MustCompile(`ticket=([A-Za-z0-9_-]+)`).FindStringSubmatch(issued.Str(t, "command"))
	script := e.get(t, "/api/v1/agent-install/windows/script?ticket="+key[1], "")
	if script.Status != 200 {
		t.Fatal(script.Status, string(script.Body))
	}
	for _, required := range []string{"component=launcher", "codegate-agent-supervisor-", "background-bootstrap.json", "repair", "Get-FileHash", "已有配置"} {
		if !bytes.Contains(script.Body, []byte(required)) {
			t.Fatal("missing migration", required)
		}
	}
	if bytes.Contains(script.Body, []byte("%!")) {
		t.Fatal("template arguments mismatched")
	}
	if bytes.Contains(script.Body, []byte("Agent 已安装；请先完成配对")) {
		t.Fatal("existing installations cannot repair service")
	}
}
