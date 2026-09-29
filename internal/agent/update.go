package agent

import (
	"context"
	"crypto/sha256"
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
		return
	}
	idle := func() int {
		count, current := a.mgr.UpdateState()
		if current != generation { return 1 }
		return count
	}
	path, version, err := stageUpdate(ctx, a.cfg.UpdateManifestURL, a.cfg.StateDir, Version, idle)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("Agent 更新检查失败", "err", err)
		}
		return
	}
	if path != "" {
		a.log.Info("已下载并校验 Agent 更新包；等待安全切换机制", "version", version, "path", path)
	}
}

// stageUpdate never installs or executes downloaded bytes. The caller provides
// a live session count so a session created during download cancels staging.
func stageUpdate(ctx context.Context, manifestURL, stateDir, currentVersion string, sessions func() int) (string, string, error) {
	if sessions() != 0 {
		return "", "", nil
	}
	origin, err := url.Parse(manifestURL)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
		return "", "", errors.New("更新清单必须使用 HTTPS")
	}
	data, err := fetchLimited(ctx, manifestURL, 64<<10, sessions)
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

func fetchLimited(ctx context.Context, address string, limit int64, sessions func() int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
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
