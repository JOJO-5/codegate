package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const installTicketTTL = 10 * time.Minute

type installTicket struct {
	userID string
	platform string
	expires time.Time
}

type installTicketStore struct {
	mu sync.Mutex
	items map[string]installTicket
}

func newInstallTicketStore() *installTicketStore {
	return &installTicketStore{items: make(map[string]installTicket)}
}

func (t *installTicketStore) issue(userID, platform string, now time.Time) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil { return "", err }
	key := base64.RawURLEncoding.EncodeToString(buf)
	t.mu.Lock()
	t.items[key] = installTicket{userID:userID, platform:platform, expires:now.Add(installTicketTTL)}
	t.mu.Unlock()
	return key, nil
}

func (t *installTicketStore) get(key, platform string, now time.Time, consume bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.items[key]
	if !ok { return false }
	if !now.Before(value.expires) { delete(t.items, key); return false }
	if value.platform != platform { return false }
	if consume { delete(t.items, key) }
	return true
}

func (t *installTicketStore) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, value := range t.items {
		if !now.Before(value.expires) { delete(t.items, key) }
	}
}

func (s *Server) handleInstallTicket(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromContext(r.Context())
	if !ok { writeError(w, http.StatusUnauthorized, "unauthenticated", "未登录"); return }
	var body struct { OS string `json:"os"` }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payload", "系统参数无效")
		return
	}
	if body.OS != "linux" && body.OS != "darwin" && body.OS != "windows" {
		writeError(w, http.StatusBadRequest, "invalid_payload", "系统不受支持")
		return
	}
	base := s.cfg.BaseURL
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		writeError(w, http.StatusBadRequest, "https_required", "首次安装需要 Server 使用 HTTPS 公网地址")
		return
	}
	key, err := s.installTickets.issue(userID, body.OS, s.now())
	if err != nil { writeError(w, http.StatusInternalServerError, "internal", "无法生成安装票据"); return }
	link := strings.TrimRight(base, "/") + "/api/v1/agent-install/" + body.OS + "/script?ticket=" + key
	var command string
	if body.OS == "windows" {
		command = "powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \"irm '" + link + "' | iex\""
	} else {
		command = "bash -o pipefail -c \"curl -fsSL '" + link + "' | sh\""
	}
	writeJSON(w, http.StatusOK, map[string]any{"command":command, "expires_in":int(installTicketTTL.Seconds())})
}

func (s *Server) installHashes(r *http.Request, platform string) (map[string]string, error) {
	hashes := make(map[string]string)
	for _, arch := range []string{"amd64", "arm64"} {
		copyReq := r.Clone(r.Context())
		copyReq.SetPathValue("os", platform)
		copyReq.SetPathValue("arch", arch)
		f, _, _, err := s.updateArtifact(copyReq)
		if err != nil { return nil, err }
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil { return nil, err }
		hashes[arch] = hex.EncodeToString(h.Sum(nil))
	}
	return hashes, nil
}

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	platform, ticket := r.PathValue("os"), r.URL.Query().Get("ticket")
	if !s.installTickets.get(ticket, platform, s.now(), false) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "安装票据无效或已过期")
		return
	}
	hashes, err := s.installHashes(r, platform)
	if err != nil { writeError(w, http.StatusNotFound, "not_found", "该系统的 Agent 包未发布"); return }
	base := "https://" + r.Host + "/api/v1/agent-install/" + platform
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if platform == "windows" {
		fmt.Fprintf(w, `$ErrorActionPreference = 'Stop'
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
if ($arch -eq 'x64') { $arch = 'amd64' }
if ($arch -eq 'arm64') { $arch = 'arm64' }
if ($arch -notin @('amd64','arm64')) { throw "不支持的 CPU: $arch" }
$expected = if ($arch -eq 'amd64') { '%s' } else { '%s' }
$dest = Join-Path $env:LOCALAPPDATA 'CodeGate\bin\codegate-agent.exe'
if (Test-Path -LiteralPath $dest) { throw 'Agent 已安装；请先完成配对，再使用空闲更新' }
New-Item -ItemType Directory -Force -Path (Split-Path $dest) | Out-Null
$service = Join-Path $env:LOCALAPPDATA 'CodeGate\agent-autostart.ps1'
Invoke-WebRequest -Uri ('%s/service?ticket=%s') -UseBasicParsing -OutFile $service
$tmp = [IO.Path]::GetTempFileName()
try {
  Invoke-WebRequest -Uri ('%s/' + $arch + '/binary') -Headers @{ 'X-CodeGate-Install-Ticket' = '%s' } -UseBasicParsing -OutFile $tmp
  if ((Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'Agent SHA-256 校验失败' }
  Move-Item -LiteralPath $tmp -Destination $dest -Force
} finally { Remove-Item -LiteralPath $tmp -ErrorAction SilentlyContinue }
Write-Host "Agent 已安装: $dest"
Write-Host '下一步：配置 %%APPDATA%%\CodeGate\agent.json，再运行 codegate-agent.exe doctor 和 pair。'
`, hashes["amd64"], hashes["arm64"], base, ticket, base, ticket)
		return
	}
	fmt.Fprintf(w, `#!/bin/sh
set -eu
case "$(uname -s)" in
  Linux) expected_os=linux ;;
  Darwin) expected_os=darwin ;;
  *) echo '不支持的系统' >&2; exit 1 ;;
esac
[ "$expected_os" = '%s' ] || { echo '安装命令与系统不匹配' >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64; expected='%s' ;;
  aarch64|arm64) arch=arm64; expected='%s' ;;
  *) echo '不支持的 CPU' >&2; exit 1 ;;
esac
dir="$HOME/.local/bin"
mkdir -p "$dir"
[ ! -e "$dir/codegate-agent" ] || { echo 'Agent 已安装；请使用空闲更新' >&2; exit 1; }
service="$HOME/.local/share/codegate/agent-autostart.sh"
mkdir -p "$(dirname "$service")"
curl -fsSL '%s/service?ticket=%s' -o "$service"
chmod 700 "$service"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM
curl -fsSL -H 'X-CodeGate-Install-Ticket: %s' '%s/'"$arch"'/binary' -o "$tmp"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp" | cut -d ' ' -f 1)
else
  actual=$(shasum -a 256 "$tmp" | cut -d ' ' -f 1)
fi
[ "$actual" = "$expected" ] || { echo 'Agent SHA-256 校验失败' >&2; exit 1; }
install -m 0755 "$tmp" "$dir/codegate-agent"
echo "Agent 已安装: $dir/codegate-agent"
echo '下一步：创建 Agent 配置文件，运行 codegate-agent doctor 和 pair。'
`, platform, hashes["amd64"], hashes["arm64"], base, ticket, ticket, base)
}

func (s *Server) handleInstallServiceScript(w http.ResponseWriter, r *http.Request) {
	platform, ticket := r.PathValue("os"), r.URL.Query().Get("ticket")
	if !s.installTickets.get(ticket, platform, s.now(), false) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "安装票据无效或已过期")
		return
	}
	name := map[string]string{"linux":"agent-autostart-linux.sh", "darwin":"agent-autostart-macos.sh", "windows":"agent-autostart.ps1"}[platform]
	if name == "" { http.NotFound(w,r); return }
	data, err := os.ReadFile(filepath.Join("/opt/codegate/agent-install", name))
	if err != nil { writeError(w, http.StatusNotFound, "not_found", "服务脚本不可用"); return }
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleInstallBinary(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("os")
	ticket := r.Header.Get("X-CodeGate-Install-Ticket")
	if !s.installTickets.get(ticket, platform, s.now(), true) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "安装票据无效或已使用")
		return
	}
	f, info, _, err := s.updateArtifact(r)
	if err != nil { writeError(w, http.StatusNotFound, "not_found", "Agent 包不可用"); return }
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
