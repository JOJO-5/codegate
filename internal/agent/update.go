package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/updatefile"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updateManifest describes one platform artifact. The manifest and artifact
// must be served from the same trusted HTTPS origin.
type updateManifest struct {
	Signature string `json:"signature,omitempty"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
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
	status.Managed = a.OnRestart != nil && Version != "dev"
	a.updateMu.RUnlock()
	if !a.startedAt.IsZero() {
		status.InstanceStartedAt = a.startedAt.UnixMilli()
	}
	status.RestartScheduled = a.restartRequested.Load()
	status.CanCancelRestart = status.Managed && status.State != "switching"
	if a.mgr != nil {
		status.BlockingSessions = a.mgr.Count()
	}
	status.BlockingOperations, _ = a.activityState()
	status.BlockingWeb = a.webActive()
	var failure protocol.AgentUpdateFailure
	if updatefile.ReadJSON(filepath.Join(a.cfg.StateDir, "updates", "last-failure.json"), &failure) == nil && failure.OccurredAt > 0 {
		status.LastFailure = &failure
	}

	return status
}

func (a *Agent) setUpdateStatus(state, version, detail string) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.updateStatus.Enabled = a.cfg.UpdateEnabled
	a.updateStatus.State, a.updateStatus.Version, a.updateStatus.Detail = state, version, detail
	if state == "checking" {
		a.updateStatus.CheckedAt = time.Now().UnixMilli()
	}
}

// updateLoop owns staged state. Downloads are independent of activity; only
// switching or restarting crosses the idle barrier.
func (a *Agent) updateLoop(ctx context.Context) {
	if a.cfg.UpdateEnabled {
		a.checkUpdate(ctx)
	}
	interval := a.cfg.UpdateInterval.Std()
	if interval <= 0 {
		interval = time.Hour
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	schedule := func() {
		a.updateMu.Lock()
		if a.cfg.UpdateEnabled {
			a.updateStatus.NextCheckAt = time.Now().Add(interval).UnixMilli()
		} else {
			a.updateStatus.NextCheckAt = 0
		}
		a.updateMu.Unlock()
		timer.Reset(interval)
	}
	schedule()
	idleTicker := time.NewTicker(5 * time.Second)
	defer idleTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.updateWake:
			a.checkUpdate(ctx)
			schedule()
		case <-timer.C:
			if a.cfg.UpdateEnabled {
				a.checkUpdate(ctx)
			}
			schedule()
		case <-idleTicker.C:
			a.applyStagedUpdate()
		}
	}
}

func (a *Agent) checkUpdate(ctx context.Context) {
	a.updateMu.Lock()
	a.updateStatus.NextCheckAt = 0
	a.updateMu.Unlock()
	a.restartReady = false
	a.setUpdateStatus("checking", a.stagedVersion, "正在检查并下载更新")
	manifestURL, err := a.updateManifestURL()
	if err != nil {
		a.setUpdateStatus("error", a.stagedVersion, err.Error())
		return
	}
	// Activity may continue while bytes are downloaded and verified.
	path, version, err := stageUpdate(ctx, manifestURL, a.cfg.StateDir, Version, func() int { return 0 }, a.id, a.cfg.UpdatePublicKey)
	if err != nil {
		if ctx.Err() == nil {
			a.setUpdateStatus("error", a.stagedVersion, err.Error())
			a.log.Warn("Agent 更新检查失败", "err", err)
		}
		// Keep a previously verified package, but never restart after a failed
		// check with no package: the failure must remain visible.
		if a.stagedPath != "" {
			a.applyStagedUpdate()
		}
		return
	}
	if path != "" {
		if a.OnStagedUpdate != nil {
			if err := a.OnStagedUpdate(path, version); err != nil {
				a.setUpdateStatus("error", version, err.Error())
				return
			}
		}
		a.stagedPath, a.stagedVersion = path, version
		a.setUpdateStatus("staged", version, "更新已下载并校验，下次启动也可安装")
	} else if a.stagedPath == "" {
		a.setUpdateStatus("current", "", "当前未发现新版本")
	}
	a.restartReady = true
	a.applyStagedUpdate()
}

func (a *Agent) applyStagedUpdate() {
	a.updateIntentMu.Lock()
	defer a.updateIntentMu.Unlock()
	if a.currentUpdateStatus().State == "switching" {
		return
	}
	var err error
	if a.stagedPath != "" && a.OnVerifiedUpdate != nil {
		err = a.OnVerifiedUpdate(a.stagedPath, a.stagedVersion)
	} else if a.restartRequested.Load() && a.restartReady && a.OnRestart != nil {
		err = a.OnRestart()
	} else {
		return
	}
	if err != nil {
		state := "error"
		if errors.Is(err, ErrUpdateBusy) {
			state = "waiting"
		}
		a.setUpdateStatus(state, a.stagedVersion, err.Error())
		return
	}
	a.restartRequested.Store(false)
	a.setUpdateStatus("switching", a.stagedVersion, "正在重启，等待守护进程确认")
}

// Serializes cancellation with the final idle barrier; downloads and automatic
// installation remain enabled. Once switching starts it cannot be cancelled.
func (a *Agent) cancelRequestedRestart() error {
	a.updateIntentMu.Lock()
	defer a.updateIntentMu.Unlock()
	if a.currentUpdateStatus().State == "switching" {
		return errors.New("Agent 已开始重启，无法取消")
	}
	a.restartRequested.Store(false)
	status := a.currentUpdateStatus()
	if status.State == "waiting" && status.Version == "" {
		a.setUpdateStatus("current", "", "手动重启安排已取消")
	}
	return nil
}

func (a *Agent) onUpdateControl(env *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.AgentUpdateControl](env)
	if err != nil || (p.Action != "check" && p.Action != "restart" && p.Action != "cancel_restart") {
		a.replyError(env, errors.New("更新操作无效"))
		return
	}
	if a.OnRestart == nil || Version == "dev" {
		a.replyError(env, errors.New("需要使用正式版 supervise 服务运行 Agent"))
		return
	}
	if p.Action == "cancel_restart" {
		if err := a.cancelRequestedRestart(); err != nil {
			a.replyError(env, err)
			return
		}
	}
	// Flush the acknowledgement before allowing the loop to stop this child.
	reply, err := protocol.NewReply(env, protocol.TypeAgentUpdateAccepted, nil)
	if err != nil {
		return
	}
	raw, err := protocol.Encode(reply)
	if err != nil {
		return
	}
	a.connMu.RLock()
	conn := a.conn
	a.connMu.RUnlock()
	if conn == nil || conn.SendTextAndWait(raw) != nil {
		return
	}
	if p.Action == "restart" {
		a.updateIntentMu.Lock()
		a.restartRequested.Store(true)
		a.updateIntentMu.Unlock()
	}
	if p.Action == "cancel_restart" {
		return
	}
	select {
	case a.updateWake <- struct{}{}:
	default:
	}
}

func (a *Agent) updateManifestURL() (string, error) {
	u, err := url.Parse(a.cfg.ServerURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" {
		return "", errors.New("更新仅支持 HTTPS")
	}
	u.Path = "/api/v1/agent-updates/" + runtime.GOOS + "/" + runtime.GOARCH + "/manifest"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// stageUpdate never installs or executes downloaded bytes. The caller provides
// a live session count so a session created during download cancels staging.
func stageUpdate(ctx context.Context, manifestURL, stateDir, currentVersion string, sessions func() int, identity *Identity, pinnedKeys ...string) (string, string, error) {
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
	var failure protocol.AgentUpdateFailure
	if updatefile.ReadJSON(filepath.Join(stateDir, "updates", "last-failure.json"), &failure) == nil && failure.Version == m.Version && failure.RetryAfter > time.Now().UnixMilli() {
		return "", "", fmt.Errorf("版本 %s 曾升级失败，退避至 %s", m.Version, time.UnixMilli(failure.RetryAfter).Format(time.RFC3339))
	}
	if m.Size <= 0 || m.Size > maxUpdateSize {
		return "", "", errors.New("更新包大小无效")
	}
	hash, err := hex.DecodeString(m.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return "", "", errors.New("更新包 SHA-256 无效")
	}
	if len(pinnedKeys) > 0 && pinnedKeys[0] != "" {
		key, keyErr := base64.StdEncoding.DecodeString(pinnedKeys[0])
		sig, sigErr := base64.StdEncoding.DecodeString(m.Signature)
		if keyErr != nil || len(key) != ed25519.PublicKeySize || sigErr != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, protocol.ReleaseSigningPayload(m.Version, m.OS, m.Arch, m.SHA256, m.Size), sig) {
			return "", "", errors.New("发布签名无效或缺失，更新已拒绝")
		}
	}
	artifactURL, err := url.Parse(m.URL)
	if err != nil || artifactURL.Scheme != "https" || artifactURL.Host != origin.Host ||
		artifactURL.User != nil || artifactURL.Fragment != "" {
		return "", "", errors.New("更新包必须与清单位于同一个 HTTPS 主机")
	}
	dir := filepath.Join(stateDir, "updates")
	if err := updatefile.EnsureDir(dir); err != nil {
		return "", "", err
	}
	name := "codegate-agent-" + m.Version + "-" + m.OS + "-" + m.Arch
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	// Recheck an existing artifact rather than trusting its filename.
	if stat, err := os.Lstat(target); err == nil && stat.Mode().IsRegular() && stat.Size() == m.Size {
		if existing, err := os.ReadFile(target); err == nil {
			sum := sha256.Sum256(existing)
			if strings.EqualFold(hex.EncodeToString(sum[:]), m.SHA256) {
				if sessions() != 0 {
					return "", "", ErrUpdateBusy
				}
				if err := os.Chmod(target, 0700); err != nil {
					return "", "", err
				}
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
	if identity != nil {
		signUpdateRequest(req, identity)
	}
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
	if err := updatefile.Replace(tmp.Name(), target); err != nil {
		return "", "", err
	}

	return target, m.Version, nil
}

func fetchLimited(ctx context.Context, address string, limit int64, sessions func() int, identity *Identity) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		signUpdateRequest(req, identity)
	}
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
			if item == "" {
				return parts, false
			}
			for _, c := range item {
				if c < '0' || c > '9' {
					return parts, false
				}
			}
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
