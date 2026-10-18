package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
)

type liveControl interface {
	close()
	call(string, any, any) error
	readThread(string) (nativeThread, nativeTurn, error)
	usageAllowed() (bool, error)
}
type liveEventSnapshot struct {
	Subscription   *bool  `json:"subscription,omitempty"`
	CredentialRoot string `json:"credential_root,omitempty"`
	Model          struct {
		ProviderID string `json:"providerID"`
	} `json:"model,omitempty"`
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	TurnID     string `json:"turn_id"`
	State      string `json:"state"`
	Code       string `json:"code,omitempty"`
	At         int64  `json:"at"`
	ResetAt    int64  `json:"reset_at,omitempty"`
	PromptHash string `json:"prompt_hash,omitempty"`
}
type eventControl struct {
	a                  *Agent
	sess               *session.Session
	ctx                context.Context
	path, marker, tool string
	p                  protocol.TaskRunPayload
	last               liveEventSnapshot
	revision           uint64
}

func (a *Agent) openLiveControl(ctx context.Context, s protocol.SessionSummary, env []string, p protocol.TaskRunPayload) (liveControl, error) {
	tool := quotaProvider(s.Command, s.Args)
	if tool == "codex" {
		return openCodexControl(ctx, codexHome(env))
	}
	if tool == "opencode" {
		_, cmd, err := a.recoveryCommand(s)
		if err != nil {
			return nil, err
		}
		v2, err := isOpenCodeV2(ctx, cmd, env, s.Cwd)
		if err != nil {
			return nil, err
		}
		if v2 {
			client, err := connectOpenCodeV2(ctx, cmd, env, s.Cwd)
			if err != nil {
				return nil, err
			}
			return &openCodeV2Control{client: client, ctx: ctx, a: a, p: p}, nil
		}
	}
	if tool == "" {
		tool = nativeTool(ResolvedCommand{Command: s.Command, Args: s.Args})
	}
	if s.Recovery == nil {
		return nil, errors.New("缺少原生控制配置")
	}
	if _, e := uuid.Parse(s.Recovery.ControlKey); e != nil {
		return nil, errors.New("此旧终端未安装原生事件桥，请恢复原对话启动一次新版终端")
	}
	marker := filepath.Join(a.cfg.StateDir, "conversation-markers", s.Recovery.ControlKey+".json")
	sess, ok := a.mgr.Get(parseUUID(s.SessionID))
	if !ok {
		return nil, errors.New("原终端不存在")
	}
	return &eventControl{a: a, sess: sess, ctx: ctx, path: marker + ".live." + s.Recovery.NativeID + ".json", marker: marker, tool: tool, p: p, revision: sess.InputRevision()}, nil
}
func (c *eventControl) close() {}
func readLiveEvent(path string) (liveEventSnapshot, error) {
	var v liveEventSnapshot
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Size() > 16<<10 {
		return v, errors.New("原CLI尚未提供结构化回合状态，请先启动任务或更新CLI")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	if e != nil || v.TurnID == "" || v.At <= 0 {
		return v, errors.New("原生回合事件无效")
	}
	return v, nil
}
func (c *eventControl) readThread(id string) (nativeThread, nativeTurn, error) {
	v, e := readLiveEvent(c.path)
	var thread nativeThread
	var turn nativeTurn
	if e != nil {
		return thread, turn, e
	}
	if v.ID != id || !sameDirectory(v.Cwd, c.sess.Cwd) {
		return thread, turn, errors.New("原生事件身份或工作目录不一致")
	}
	c.last = v
	thread.ID = v.ID
	thread.Cwd = v.Cwd
	thread.Status.Type = "idle"
	turn.ID = v.TurnID
	turn.Status = v.State
	turn.RetryAt = v.ResetAt
	if v.State == "inProgress" {
		thread.Status.Type = "active"
	}
	if v.Code != "" {
		info, _ := json.Marshal(v.Code)
		turn.Error = &struct {
			Info json.RawMessage `json:"codexErrorInfo"`
		}{info}
	}
	return thread, turn, nil
}
func (c *eventControl) usageAllowed() (bool, error) {
	// Use native/local credentials only for the same CLI; unavailable usage is
	// never advertised as recovered. A user-configured bounded retry is used
	// only when the provider supplies no queryable subscription API.
	if c.tool == "claude" && c.last.Subscription != nil && *c.last.Subscription && filepath.IsAbs(c.last.CredentialRoot) {
		env := replaceEnv(BuildEnv(c.a.cfg, 80, 24), "CLAUDE_CONFIG_DIR", c.last.CredentialRoot)
		windows, message := claudeQuota(c.ctx, env, quotaHTTPClient())
		if message == "" && len(windows) > 0 {
			q := protocol.QuotaResult{Providers: []protocol.ProviderQuota{{Status: "ok", Windows: windows}}}
			return blockedUntil(q, time.Now()) == 0, nil
		}
		return false, errors.New(message)
	}
	if c.tool == "opencode" && c.last.Model.ProviderID == "opencode-go" {
		q, e := c.a.readProviderQuota(c.ctx, c.tool)
		if e == nil && len(q.Providers) == 1 && q.Providers[0].Status == "ok" && len(q.Providers[0].Windows) > 0 {
			return blockedUntil(q, time.Now()) == 0, nil
		}
		return false, errors.New("当前订阅额度暂不可查询，继续等待，不会假定恢复")
	}
	at := c.last.ResetAt
	if at == 0 {
		delay := c.p.QuotaWaitMinutes
		if delay == 0 {
			delay = 300
		}
		at = c.last.At + int64(delay)*60000
	}
	return time.Now().UnixMilli() >= at, nil
}
func (c *eventControl) call(method string, params any, out any) error {
	raw, _ := json.Marshal(params)
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ClientID string `json:"clientUserMessageId"`
		Input    []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if e := json.Unmarshal(raw, &p); e != nil {
		return e
	}
	if p.ThreadID != c.last.ID {
		return errors.New("原生控制目标不一致")
	}
	if c.tool == "claude" {
		if method == "turn/interrupt" {
			if _, e := c.sess.WriteAutomation(c.revision, []byte{3}); e != nil {
				return e
			}
			return json.Unmarshal([]byte(`{}`), out)
		}
		if method != "turn/start" || len(p.Input) != 1 || strings.ContainsAny(p.Input[0].Text, "\x00\x1b\r") || strings.HasPrefix(strings.TrimSpace(p.Input[0].Text), "/") {
			return errors.New("CLI继续指令包含不支持的控制字符")
		}
		input := []byte("\x1b[200~" + p.Input[0].Text + "\x1b[201~\r")
		revision := c.revision
		if _, e := c.sess.WriteAutomation(revision, input); e != nil {
			return e
		}
		// A native UserPromptSubmit hook must acknowledge the exact prompt. A
		// partial/ambiguous PTY submission stays preparing and is never repeated.
		wanted := promptDigest(p.Input[0].Text)
		for {
			select {
			case <-c.ctx.Done():
				return errors.New("CLI提交回执超时")
			case <-time.After(50 * time.Millisecond):
			}
			v, e := readLiveEvent(c.path)
			if e == nil && v.ID == c.last.ID && v.TurnID != c.last.TurnID && v.PromptHash == wanted {
				return json.Unmarshal([]byte(`{"turn":{"id":`+quoteJSON(v.TurnID)+`,"status":"inProgress"}}`), out)
			}
		}
	}
	if method != "turn/start" && method != "turn/interrupt" {
		return errors.New("不支持的原生控制请求")
	}
	request := map[string]any{"id": p.ClientID, "native_id": p.ThreadID, "expected_turn": c.last.TurnID, "action": method, "deadline": time.Now().Add(5 * time.Second).UnixMilli()}
	if method == "turn/interrupt" {
		request["id"] = uuid.NewString()
	}
	if len(p.Input) == 1 {
		request["prompt"] = p.Input[0].Text
	}
	if e := writeLiveJSON(c.marker+".request.json", request); e != nil {
		return e
	}
	for {
		select {
		case <-c.ctx.Done():
			return errors.New("原生控制回执超时")
		case <-time.After(50 * time.Millisecond):
		}
		b, e := os.ReadFile(c.marker + ".ack.json")
		if e != nil || len(b) > 16<<10 {
			continue
		}
		var ack struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			TurnID string `json:"turn_id"`
		}
		if json.Unmarshal(b, &ack) != nil || ack.ID != request["id"] || ack.State == "preparing" {
			continue
		}
		if ack.State != "ok" {
			return errors.New("原CLI拒绝自动提交，状态已变化或接口不支持")
		}
		return json.Unmarshal([]byte(`{"turn":{"id":`+quoteJSON(ack.TurnID)+`,"status":"inProgress"}}`), out)
	}
}
func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
func promptDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func writeLiveJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".live-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(tmp, path)
}

// RecordLiveEvent is called by Claude's native lifecycle hooks. Store only
// identity, a prompt hash and an error category; never transcripts or secrets.
func RecordLiveEvent(args []string, input io.Reader) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) || filepath.Base(filepath.Dir(args[0])) != "conversation-markers" {
		return errors.New("invalid live marker")
	}
	if _, e := uuid.Parse(strings.TrimSuffix(filepath.Base(args[0]), ".json")); e != nil {
		return e
	}
	var h struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Event     string `json:"hook_event_name"`
		Error     string `json:"error"`
		Prompt    string `json:"prompt"`
	}
	if e := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&h); e != nil {
		return e
	}
	if !nativeIDPattern.MatchString(h.SessionID) || !filepath.IsAbs(h.Cwd) {
		return errors.New("invalid native hook identity")
	}
	path := args[0] + ".live." + h.SessionID + ".json"
	old, _ := readLiveEvent(path)
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".claude")
	}
	subscription := os.Getenv("ANTHROPIC_API_KEY") == "" && (os.Getenv("ANTHROPIC_BASE_URL") == "" || os.Getenv("ANTHROPIC_BASE_URL") == "https://api.anthropic.com")
	v := liveEventSnapshot{Subscription: &subscription, CredentialRoot: root, ID: h.SessionID, Cwd: h.Cwd, TurnID: old.TurnID, PromptHash: old.PromptHash, At: time.Now().UnixMilli()}
	switch h.Event {
	case "UserPromptSubmit":
		v.TurnID = uuid.NewString()
		v.PromptHash = promptDigest(h.Prompt)
		v.State = "inProgress"
	case "Stop":
		v.State = "completed"
	case "StopFailure":
		v.State = "failed"
		switch h.Error {
		case "rate_limit":
			v.Code = "usageLimitExceeded"
		case "server_error", "overloaded_error":
			v.Code = "serverOverloaded"
		case "network", "network_error":
			v.Code = "responseStreamConnectionFailed"
		default:
			v.Code = "other"
		}
	default:
		return nil
	}
	if v.TurnID == "" {
		return nil
	}
	return writeLiveJSON(path, v)
}
