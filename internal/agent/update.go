package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"github.com/jojo/codegate/internal/protocol"
	"strings"
	"time"
)

// updateManifest describes one platform artifact. The manifest and artifact
// must be served from the same trusted HTTPS origin.
type updateManifest struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

const maxUpdateSize int64 = 100 << 20

var updateHTTPClient = &http.Client{
	Timeout: 3 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("更新地址不允许重定向")
	},
}

// currentUpdateStatus returns a snapshot for the next authenticated heartbeat.
func (a *Agent) currentUpdateStatus() protocol.AgentUpdateStatus {
	a.updateMu.RLock()
	status := a.updateStatus
	a.updateMu.RUnlock()
	data, err := os.ReadFile(filepath.Join(a.cfg.StateDir, "updates", "last-failure.json"))
	if err == nil && len(data) <= 4096 {
		var failure protocol.AgentUpdateFailure
		if json.Unmarshal(data, &failure) == nil && failure.OccurredAt > 0 { status.LastFailure = &failure }
	}
	return status
}

func (a *Agent) setUpdateStatus(state, version, detail string) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.updateStatus = protocol.AgentUpdateStatus{
		Enabled: true, State: state, Version: version, Detail: detail,
		CheckedAt: time.Now().UnixMilli(),
	}
}

func (a *Agent) updateLoop(ctx context.Context) {
	a.checkUpdate(ctx)
	ticker := time.NewTicker(a.cfg.UpdateInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkUpdate(ctx)
		}
	}
}

func (a *Agent) checkUpdate(ctx context.Context) {
	// Count includes retained exited sessions: their scrollback must remain
	// available until the user closes them or retention reaps them.
	count, generation := a.mgr.UpdateState()
	if count != 0 {
		a.setUpdateStatus("waiting", "", "有会话正在运行或等待回看")
		return
	}
	idle := func() int {
		count, current := a.mgr.UpdateState()
		if current != generation { return 1 }
		return count
	}
	a.setUpdateStatus("checking", "", "")
	manifestURL, err := a.updateManifestURL()
	if err != nil { a.setUpdateStatus("error", "", "更新地址无效"); a.log.Warn("Agent 更新地址无效", "err", err); return }
	path, version, err := stageUpdate(ctx, manifestURL, a.cfg.StateDir, Version, idle, a.id)
	if err != nil {
		if ctx.Err() == nil {
			a.setUpdateStatus("error", "", err.Error())
			a.log.Warn("Agent 更新检查失败", "err", err)
		}
		return
	}
	if path == "" {
		if idle() != 0 { a.setUpdateStatus("waiting", "", "检查期间会话状态发生变化")
		} else { a.setUpdateStatus("current", "", "当前未发现新版本") }
		return
	}
	if path != "" {
		a.setUpdateStatus("staged", version, "更新包已校验")
		a.log.Info("已下载并校验 Agent 更新包", "version", version, "path", path)
		if a.OnVerifiedUpdate != nil {
			if err := a.OnVerifiedUpdate(path, version); err != nil {
				a.setUpdateStatus("error", version, err.Error())
				a.log.Warn("更新切换准备失败", "err", err)
			} else {
				a.setUpdateStatus("switching", version, "等待守护进程确认新版本")
			}
		}
	}
}

func (a *Agent) updateManifestURL() (string, error) {
	u, err := url.Parse(a.cfg.ServerURL)
	if err != nil { return "", err }
	if u.Scheme == "wss" { u.Scheme = "https" }
	if u.Scheme != "https" { return "", errors.New("更新仅支持 HTTPS") }
	u.Path = "/api/v1/agent-updates/" + runtime.GOOS + "/" + runtime.GOARCH + "/manifest"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// stageUpdate never installs or executes downloaded bytes. The caller provides
// a live session count so a session created during download cancels staging.
func stageUpdate(ctx context.Context, manifestURL, stateDir, currentVersion string, sessions func() int, identity *Identity) (string, string, error) {
	if sessions() != 0 {
		return "", "", nil
	}
	origin, err := url.Parse(manifestURL)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
		return "", "", errors.New("更新清单必须使用 HTTPS")
	}
	data, err := fetchLimited(ctx, manifestURL, 64<<10, sessions, identity)
	if err != nil {
		return "", "", err
	}
	var m updateManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", "", fmt.Errorf("更新清单格式错误: %w", err)
	}
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH || !newerVersion(m.Version, currentVersion) {
		return "", "", nil
	}
	if m.Size <= 0 || m.Size > maxUpdateSize {
		return "", "", errors.New("更新包大小无效")
	}
	hash, err := hex.DecodeString(m.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return "", "", errors.New("更新包 SHA-256 无效")
	}
	artifactURL, err := url.Parse(m.URL)
	if err != nil || artifactURL.Scheme != "https" || artifactURL.Host != origin.Host ||
		artifactURL.User != nil || artifactURL.Fragment != "" {
		return "", "", errors.New("更新包必须与清单位于同一个 HTTPS 主机")
	}
	dir := filepath.Join(stateDir, "updates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	name := "codegate-agent-" + m.Version + "-" + m.OS + "-" + m.Arch
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	// Recheck an existing artifact rather than trusting its filename.
	if stat, err := os.Stat(target); err == nil && stat.Size() == m.Size {
		if existing, err := os.ReadFile(target); err == nil {
			sum := sha256.Sum256(existing)
			if strings.EqualFold(hex.EncodeToString(sum[:]), m.SHA256) {
				return target, m.Version, nil
			}
		}
	}
	if sessions() != 0 {
		return "", "", nil
	}
	tmp, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o700); err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return "", "", err
	}
	if identity != nil { signUpdateRequest(req, identity) }
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("更新包 HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var size int64
	for {
		if sessions() != 0 {
			return "", "", errors.New("下载期间新建了会话，更新已取消")
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > m.Size || size > maxUpdateSize {
				return "", "", errors.New("更新包超过清单声明的大小")
			}
			if _, err := h.Write(buf[:n]); err != nil {
				return "", "", err
			}
			if _, err := tmp.Write(buf[:n]); err != nil {
				return "", "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", "", readErr
		}
	}
	if sessions() != 0 {
		return "", "", errors.New("下载期间新建了会话，更新已取消")
	}
	if size != m.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), m.SHA256) {
		return "", "", errors.New("更新包大小或 SHA-256 校验失败")
	}
	if err := tmp.Sync(); err != nil {
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	// Windows cannot rename over an existing destination.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return "", "", err
	}
	return target, m.Version, nil
}

func fetchLimited(ctx context.Context, address string, limit int64, sessions func() int, identity *Identity) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if identity != nil { signUpdateRequest(req, identity) }
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("更新清单 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if sessions() != 0 {
		return nil, errors.New("更新检查期间新建了会话")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("更新清单过大")
	}
	return data, nil
}

func signUpdateRequest(req *http.Request, identity *Identity) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	payload := protocol.UpdateSigningPayload(req.URL.Host, req.URL.Path, timestamp)
	req.Header.Set("X-CodeGate-Device", identity.DeviceID)
	req.Header.Set("X-CodeGate-Timestamp", timestamp)
	req.Header.Set("X-CodeGate-Signature", base64.StdEncoding.EncodeToString(identity.Sign(payload)))
}

func newerVersion(candidate, current string) bool {
	parse := func(s string) ([3]int, bool) {
		var parts [3]int
		items := strings.Split(strings.TrimPrefix(s, "v"), ".")
		if len(items) != 3 {
			return parts, false
		}
		for i, item := range items {
			n, err := strconv.Atoi(item)
			if err != nil || n < 0 {
				return parts, false
			}
			parts[i] = n
		}
		return parts, true
	}
	next, ok := parse(candidate)
	if !ok {
		return false
	}
	now, ok := parse(current)
	if !ok {
		return false
	}
	for i := range next {
		if next[i] != now[i] {
			return next[i] > now[i]
		}
	}
	return false
}
