package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

const maxAgentUpdateBytes int64 = 100 << 20

var updateVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func validAgentPlatform(goos, arch string) bool {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return false
	}
	return arch == "amd64" || arch == "arm64"
}

// updateRequestAllowed authenticates a read-only request with the paired
// device's existing Ed25519 key. TLS protects the short-lived signed headers.
func (s *Server) updateRequestAllowed(r *http.Request) bool {
	deviceID := r.Header.Get("X-CodeGate-Device")
	timestamp := r.Header.Get("X-CodeGate-Timestamp")
	signature := r.Header.Get("X-CodeGate-Signature")
	if deviceID == "" || timestamp == "" || signature == "" {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || seconds < s.now().Add(-30*time.Second).Unix() || seconds > s.now().Add(30*time.Second).Unix() {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	device, err := s.store.DeviceByID(r.Context(), deviceID)
	if err != nil || !device.Paired() || device.RevokedAt != nil ||
		device.Platform != r.PathValue("os") || device.Arch != r.PathValue("arch") {
		return false
	}
	return ed25519.Verify(device.PublicKey,
		protocol.UpdateSigningPayload(r.Host, r.URL.Path, timestamp), sig)
}

func (s *Server) updateArtifact(r *http.Request) (*os.File, os.FileInfo, string, error) {
	if s.cfg.AgentUpdatesDir == "" || !validAgentPlatform(r.PathValue("os"), r.PathValue("arch")) {
		return nil, nil, "", os.ErrNotExist
	}
	dir := filepath.Join(s.cfg.AgentUpdatesDir, r.PathValue("os"), r.PathValue("arch"))
	versionBytes, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		return nil, nil, "", err
	}
	version := strings.TrimSpace(string(versionBytes))
	if !updateVersionPattern.MatchString(version) {
		return nil, nil, "", errors.New("更新版本号无效")
	}
	name := "codegate-agent"
	if r.PathValue("os") == "windows" {
		name += ".exe"
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, nil, "", err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAgentUpdateBytes {
		f.Close()
		return nil, nil, "", errors.New("更新包不是有效的普通文件")
	}
	return f, info, version, nil
}

func (s *Server) handleAgentUpdateManifest(w http.ResponseWriter, r *http.Request) {
	if !s.updateRequestAllowed(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "设备认证失败")
		return
	}
	f, info, version, err := s.updateArtifact(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "更新包不可用")
		return
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "读取更新包失败")
		return
	}
	downloadPath := "/api/v1/agent-updates/" + r.PathValue("os") + "/" + r.PathValue("arch") + "/binary"
	// Preserve the Host received through the reverse proxy: it is also bound
	// into the Agent's signature and must match the Agent's Server URL.
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version, "os": r.PathValue("os"), "arch": r.PathValue("arch"),
		"url": "https://" + r.Host + downloadPath,
		"sha256": hex.EncodeToString(sum.Sum(nil)), "size": info.Size(),
	})
}

func (s *Server) handleAgentUpdateBinary(w http.ResponseWriter, r *http.Request) {
	if !s.updateRequestAllowed(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "设备认证失败")
		return
	}
	f, info, _, err := s.updateArtifact(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "更新包不可用")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ServeContent handles HEAD/range requests only if configured; this route
	// is GET-only and the Agent verifies the exact size and digest.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
