package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// Refresh is read-only. No prompts, credential refresh, login or browser cookies.
// Concurrent requests share a short cache, including failures, to bound native work.
func (a *Agent) onQuotaRead(req *protocol.Envelope) {
	a.quotaMu.Lock()
	defer a.quotaMu.Unlock()
	if time.Since(a.quotaChecked) >= 15*time.Second {
		cfg := a.commandConfig()
		env := BuildEnv(cfg, 80, 24)
		result := protocol.QuotaResult{Providers: []protocol.ProviderQuota{}}
		for _, provider := range []string{"codex", "claude", "opencode"} {
			q := protocol.ProviderQuota{Provider: provider, Status: "unavailable", Windows: []protocol.QuotaWindow{}}
			var command *ResolvedCommand
			for _, spec := range cfg.AllowedCommands {
				c, err := cfg.ResolveCommand(spec.ID, "", nil, false)
				if err == nil && nativeTool(c) == provider {
					command = &c
					break
				}
			}
			if command == nil {
				q.Message = "此设备未授权这个 CLI"
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				switch provider {
				case "codex":
					q.Source = "Codex 本机账号 · 官方 app-server"
					q.Windows, q.Message = codexQuota(ctx, *command, env)
				case "claude":
					q.Source = "Claude 本机 OAuth · 非公开用量接口"
					q.Windows, q.Message = claudeQuota(ctx, env, quotaHTTPClient())
				case "opencode":
					q.Source = "OpenCode Go 本机账号"
					q.Windows, q.Message = openCodeQuota(ctx, env, quotaHTTPClient())
				}
				cancel()
			}
			q.CheckedAt = time.Now().Unix()
			if q.Message == "" && len(q.Windows) > 0 {
				q.Status = "ok"
			}
			result.Providers = append(result.Providers, q)
		}
		a.quotaCache = result
		a.quotaChecked = time.Now()
	}
	a.reply(req, protocol.TypeQuotaResult, a.quotaCache)
}

func quotaHTTPClient() *http.Client {
	// Never redirect bearer credentials to an arbitrary location.
	return &http.Client{Timeout: 6 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
func quotaFile(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(dst)
}
func quotaGet(ctx context.Context, client *http.Client, endpoint, token string, claude bool, out any) string {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "请求配置无效"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if claude {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "查询失败或超时，请稍后刷新"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 401:
		return "本机登录已过期，请在 CLI 中重新登录"
	case 403:
		return "当前账号无权限或未订阅此服务"
	case 429:
		return "查询过于频繁，请稍后刷新"
	default:
		return "额度服务暂不可用"
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
		return "额度服务返回了无法识别的数据"
	}
	return ""
}
func quotaWindow(label string, used *float64, reset int64) (protocol.QuotaWindow, bool) {
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 100 {
		return protocol.QuotaWindow{}, false
	}
	if reset < 0 {
		reset = 0
	}
	return protocol.QuotaWindow{Label: label, UsedPercent: *used, ResetsAt: reset}, true
}
func codexQuota(ctx context.Context, c ResolvedCommand, env []string) ([]protocol.QuotaWindow, string) {
	args := c.Args
	if strings.EqualFold(filepath.Base(c.Command), "cmd.exe") && len(args) >= 2 && strings.EqualFold(args[0], "/c") {
		args = args[2:]
	} else if len(args) > 0 {
		return nil, "自定义 CLI 参数需在 CLI 内查看额度"
	}
	if len(args) > 0 {
		return nil, "自定义 CLI 参数需在 CLI 内查看额度"
	}
	// Use only a locally approved executable, retaining the Windows npm shim.
	cmdArgs := []string{"app-server"}
	if strings.EqualFold(filepath.Base(c.Command), "cmd.exe") {
		cmdArgs = append(append([]string{}, c.Args[:2]...), cmdArgs...)
	}
	cmd := exec.Command(c.Command, cmdArgs...)
	configureQuotaProcess(cmd)
	cmd.Env = env
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, "无法启动本机 Codex"
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "无法启动本机 Codex"
	}
	if cmd.Start() != nil {
		return nil, "Codex 不可用，请安装或更新 CLI"
	}
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { input.Close(); stopQuotaProcess(cmd); output.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	defer func() { close(done); stop(); cmd.Wait() }()
	encoder := json.NewEncoder(input)
	if encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "codegate", "version": Version}}}) != nil {
		return nil, "Codex 查询失败"
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var frame struct {
			ID     int             `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result struct {
				RateLimits struct {
					Primary   *codexQuotaLane `json:"primary"`
					Secondary *codexQuotaLane `json:"secondary"`
				} `json:"rateLimits"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		if frame.ID == 1 {
			if len(frame.Error) > 0 {
				return nil, "Codex 版本不支持额度查询，请更新 CLI"
			}
			if encoder.Encode(map[string]any{"method": "initialized"}) != nil {
				return nil, "Codex 查询失败"
			}
			if encoder.Encode(map[string]any{"id": 2, "method": "account/rateLimits/read"}) != nil {
				return nil, "Codex 查询失败"
			}
		}
		if frame.ID == 2 {
			if len(frame.Error) > 0 {
				return nil, "请使用 ChatGPT 账号登录 Codex；API Key 无订阅额度"
			}
			windows := []protocol.QuotaWindow{}
			for _, lane := range []*codexQuotaLane{frame.Result.RateLimits.Primary, frame.Result.RateLimits.Secondary} {
				if lane == nil {
					continue
				}
				label := "额度窗口"
				if lane.Minutes > 0 {
					label = fmt.Sprintf("%d 分钟窗口", lane.Minutes)
					if lane.Minutes%60 == 0 {
						label = fmt.Sprintf("%d 小时窗口", lane.Minutes/60)
					}
					if lane.Minutes%1440 == 0 {
						label = fmt.Sprintf("%d 天窗口", lane.Minutes/1440)
					}
				}
				if w, ok := quotaWindow(label, lane.Used, lane.Reset); ok {
					windows = append(windows, w)
				}
			}
			if len(windows) == 0 {
				return nil, "当前账号没有可查询的订阅额度"
			}
			return windows, ""
		}
	}
	return nil, "Codex 查询失败或超时，请更新 CLI 后重试"
}

type codexQuotaLane struct {
	Used    *float64 `json:"usedPercent"`
	Minutes int      `json:"windowDurationMins"`
	Reset   int64    `json:"resetsAt"`
}

func claudeQuota(ctx context.Context, env []string, client *http.Client) ([]protocol.QuotaWindow, string) {
	root := nativeEnvValue(env, "CLAUDE_CONFIG_DIR")
	if root == "" {
		root = filepath.Join(nativeHome(env), ".claude")
	}
	var auth struct {
		OAuth struct {
			Token string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if quotaFile(filepath.Join(root, ".credentials.json"), &auth) != nil || auth.OAuth.Token == "" {
		return nil, "未找到本机 Claude OAuth 登录（API Key / 系统钥匙串暂不支持）"
	}
	var data struct {
		Five  *claudeQuotaLane `json:"five_hour"`
		Seven *claudeQuotaLane `json:"seven_day"`
	}
	if message := quotaGet(ctx, client, "https://api.anthropic.com/api/oauth/usage", auth.OAuth.Token, true, &data); message != "" {
		return nil, message
	}
	result := []protocol.QuotaWindow{}
	for i, lane := range []*claudeQuotaLane{data.Five, data.Seven} {
		if lane == nil {
			continue
		}
		reset, _ := time.Parse(time.RFC3339, lane.Reset)
		label := []string{"5 小时", "7 天"}[i]
		if w, ok := quotaWindow(label, lane.Used, reset.Unix()); ok {
			result = append(result, w)
		}
	}
	if len(result) == 0 {
		return nil, "当前账号没有可查询的订阅额度"
	}
	return result, ""
}

type claudeQuotaLane struct {
	Used  *float64 `json:"utilization"`
	Reset string   `json:"resets_at"`
}

func openCodeQuota(ctx context.Context, env []string, client *http.Client) ([]protocol.QuotaWindow, string) {
	root := nativeEnvValue(env, "XDG_DATA_HOME")
	if root == "" {
		root = filepath.Join(nativeHome(env), ".local", "share")
	}
	var auth map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	_ = quotaFile(filepath.Join(root, "opencode", "auth.json"), &auth)
	token := nativeEnvValue(env, "OPENCODE_API_KEY")
	if token == "" {
		if a, ok := auth["opencode-go"]; ok && a.Type == "api" {
			token = a.Key
		}
	}
	if token == "" {
		return nil, "仅支持 Go 订阅；Zen 余额及第三方模型请到服务商查看"
	}
	var data struct {
		Usage map[string]struct {
			Used  *float64 `json:"percent"`
			Reset string   `json:"resetsAt"`
		} `json:"usage"`
	}
	if message := quotaGet(ctx, client, "https://opencode.ai/zen/go/v1/usage", token, false, &data); message != "" {
		return nil, message
	}
	result := []protocol.QuotaWindow{}
	for i, key := range []string{"rolling", "weekly", "monthly"} {
		lane, ok := data.Usage[key]
		if !ok {
			continue
		}
		reset, _ := time.Parse(time.RFC3339, lane.Reset)
		if w, ok := quotaWindow([]string{"滚动窗口", "每周", "每月"}[i], lane.Used, reset.Unix()); ok {
			result = append(result, w)
		}
	}
	if len(result) == 0 {
		return nil, "服务未返回可用额度"
	}
	return result, ""
}
