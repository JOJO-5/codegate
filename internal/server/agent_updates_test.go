package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

func TestAgentUpdateRequiresPairedDeviceSignature(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()
	e.srv.cfg.AgentUpdatesDir = dir
	artifactDir := filepath.Join(dir, "linux", "amd64")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil { t.Fatal(err) }
	payload := []byte("test binary")
	if err := os.WriteFile(filepath.Join(artifactDir, "codegate-agent"), payload, 0o600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(artifactDir, "version.txt"), []byte("v1.2.4\n"), 0o600); err != nil { t.Fatal(err) }
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil { t.Fatal(err) }
	userToken := e.signup(t, "update@example.com", "testPassword123!")
	userID := e.meUserID(t, userToken)
	deviceID := "device-for-updates"
	if err := e.store.CreateDevice(context.Background(), &storage.Device{
		ID:deviceID, Name:"Update test", Platform:"linux", Arch:"amd64", PublicKey:pub,
		CreatedAt:e.clock.Now(),
	}); err != nil { t.Fatal(err) }
	if err := e.store.BindDevice(context.Background(), deviceID, userID, e.clock.Now()); err != nil { t.Fatal(err) }

	path := "/api/v1/agent-updates/linux/amd64/manifest"
	request := func(signed bool, path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, e.http.URL+path, nil)
		if err != nil { t.Fatal(err) }
		if signed {
			stamp := strconv.FormatInt(e.clock.Now().Unix(), 10)
			sig := ed25519.Sign(priv, protocol.UpdateSigningPayload(req.URL.Host, req.URL.Path, stamp))
			req.Header.Set("X-CodeGate-Device", deviceID)
			req.Header.Set("X-CodeGate-Timestamp", stamp)
			req.Header.Set("X-CodeGate-Signature", base64.StdEncoding.EncodeToString(sig))
		}
		resp, err := e.http.Client().Do(req)
		if err != nil { t.Fatal(err) }
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil { t.Fatal(err) }
		return resp, body
	}
	if resp, _ := request(false, path); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status %d", resp.StatusCode)
	}
	if resp, _ := request(true, "/api/v1/agent-updates/windows/amd64/manifest"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong platform status %d", resp.StatusCode)
	}
	resp, body := request(true, path)
	if resp.StatusCode != http.StatusOK { t.Fatalf("manifest: %d %s", resp.StatusCode, body) }
	var manifest struct { Version string `json:"version"`; SHA256 string `json:"sha256"`; Size int64 `json:"size"`; URL string `json:"url"` }
	if err := json.Unmarshal(body, &manifest); err != nil { t.Fatal(err) }
	sum := sha256.Sum256(payload)
	if manifest.Version != "v1.2.4" || manifest.Size != int64(len(payload)) || manifest.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	parsed, err := url.Parse(manifest.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != reqHost(e.http.URL) { t.Fatalf("bad artifact URL: %q", manifest.URL) }
	resp, body = request(true, parsed.Path)
	if resp.StatusCode != http.StatusOK || string(body) != string(payload) {
		t.Fatalf("binary: %d %q", resp.StatusCode, body)
	}
}

func reqHost(raw string) string { u, _ := url.Parse(raw); return u.Host }
