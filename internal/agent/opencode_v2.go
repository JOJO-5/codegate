package agent

// V2 owns execution in a shared service, independently of the TUI process.
// Credentials and endpoint discovery stay local; neither comes from the Web.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
)

type openCodeV2Location struct {
	Directory string `json:"directory"`
}
type openCodeV2Session struct {
	ID       string             `json:"id"`
	ParentID string             `json:"parentID"`
	Title    string             `json:"title"`
	Location openCodeV2Location `json:"location"`
	Outcome  string             `json:"outcome"`
	Time     struct {
		Created  int64 `json:"created"`
		Updated  int64 `json:"updated"`
		Idle     int64 `json:"idle"`
		Archived int64 `json:"archived"`
	} `json:"time"`
	Model struct {
		ProviderID string `json:"providerID"`
	} `json:"model"`
}
type openCodeV2Client struct {
	endpoint      *url.URL
	password, cwd string
	http          *http.Client
}

type openCodeV2HTTPError struct{ status int }

func (e *openCodeV2HTTPError) Error() string {
	return fmt.Sprintf("OpenCode 原生接口返回 HTTP %d", e.status)
}
func openCodeV2NotFound(err error) bool {
	var response *openCodeV2HTTPError
	return errors.As(err, &response) && response.status == 404
}

func serverHTTPClient() *http.Client {
	return &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func openCodeCLI(ctx context.Context, c ResolvedCommand, env []string, cwd string, args ...string) (string, error) {
	prefix := []string{}
	if strings.EqualFold(strings.TrimSuffix(filepath.Base(c.Command), ".exe"), "cmd") && len(c.Args) >= 2 && strings.EqualFold(c.Args[0], "/c") {
		prefix = append(prefix, c.Args[:2]...)
	}
	cmd := exec.CommandContext(ctx, c.Command, append(prefix, args...)...)
	processutil.Background(cmd)
	cmd.Dir = cwd
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("OpenCode 本机命令失败，请检查 CLI 安装和后台服务")
	}
	if len(out) > 64<<10 {
		return "", errors.New("OpenCode 本机命令输出过大")
	}
	return strings.TrimSpace(string(out)), nil
}

func isOpenCodeV2(ctx context.Context, c ResolvedCommand, env []string, cwd string) (bool, error) {
	if nativeTool(c) != "opencode" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	version, err := openCodeCLI(ctx, c, env, cwd, "--version")
	if err != nil {
		return false, err
	}
	version = strings.TrimPrefix(strings.TrimPrefix(version, "opencode "), "v")
	if strings.HasPrefix(version, "2.") {
		return true, nil
	}
	if strings.HasPrefix(version, "1.") {
		return false, nil
	}
	return false, errors.New("无法识别 OpenCode 主版本，未加载旧版插件")
}

func connectOpenCodeV2(ctx context.Context, c ResolvedCommand, env []string, cwd string) (*openCodeV2Client, error) {
	// Custom server/standalone transports need their own identity and lifecycle.
	// Refuse rather than accidentally controlling the default service instead.
	for _, arg := range c.Args {
		if arg == "--standalone" || arg == "--server" || strings.HasPrefix(arg, "--server=") {
			return nil, errors.New("OpenCode V2 原生集成需要默认本机后台服务，请使用普通 CLI 配置")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endpoint, err := openCodeCLI(ctx, c, env, cwd, "service", "status")
	if err != nil {
		return nil, err
	}
	endpoint = strings.TrimPrefix(endpoint, "running ")
	if endpoint == "stopped" || endpoint == "" {
		endpoint, err = openCodeCLI(ctx, c, env, cwd, "service", "start")
		if err != nil {
			return nil, err
		}
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("OpenCode 后台服务地址无效")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("OpenCode 后台服务必须使用本机回环地址")
	}
	password, err := openCodeCLI(ctx, c, env, cwd, "service", "get", "password")
	if err != nil {
		return nil, err
	}
	client := &openCodeV2Client{endpoint: u, password: password, cwd: cwd, http: serverHTTPClient()}
	var info struct {
		Version string `json:"version"`
	}
	if err = client.call(ctx, "GET", "/api/info", nil, &info); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(info.Version, "2.") {
		return nil, errors.New("OpenCode 后台服务尚未升级为 V2")
	}
	return client, nil
}

func (c *openCodeV2Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	target := *c.endpoint
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" {
		return errors.New("无效的 OpenCode 原生请求")
	}
	target.Path = relative.Path
	target.RawQuery = relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return errors.New("OpenCode 后台服务连接失败，执行结果需核对")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &openCodeV2HTTPError{status: res.StatusCode}
	}
	if out == nil || res.StatusCode == 204 {
		return nil
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
		return errors.New("OpenCode 原生响应格式无效")
	}
	return nil
}

func (c *openCodeV2Client) session(ctx context.Context, id string) (openCodeV2Session, error) {
	var result struct {
		Data openCodeV2Session `json:"data"`
	}
	if !nativeIDPattern.MatchString(id) {
		return result.Data, errors.New("无效的 OpenCode 对话 ID")
	}
	err := c.call(ctx, "GET", "/api/session/"+url.PathEscape(id), nil, &result)
	if err != nil {
		return result.Data, err
	}
	dir, e := filepath.EvalSymlinks(result.Data.Location.Directory)
	expected, expectedErr := filepath.EvalSymlinks(c.cwd)
	if e != nil || expectedErr != nil || result.Data.ID != id || !sameDirectory(expected, dir) {
		return result.Data, errors.New("OpenCode 对话身份或工作目录不一致")
	}
	return result.Data, nil
}

func (c *openCodeV2Client) history(ctx context.Context, cwd, target string) ([]protocol.NativeConversation, error) {
	expected, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, errors.New("无法核对 OpenCode 工作目录")
	}
	var sessions []openCodeV2Session
	if target != "" {
		s, err := c.session(ctx, target)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	} else {
		cursor := ""
		seen := map[string]bool{}
		for page := 0; page < 100; page++ {
			q := url.Values{"directory": {cwd}, "parentID": {"null"}, "limit": {"100"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			} else {
				q.Set("order", "desc")
			}
			var response struct {
				Data   []openCodeV2Session `json:"data"`
				Cursor struct {
					Next string `json:"next"`
				} `json:"cursor"`
			}
			if err := c.call(ctx, "GET", "/api/session?"+q.Encode(), nil, &response); err != nil {
				return nil, err
			}
			sessions = append(sessions, response.Data...)
			cursor = response.Cursor.Next
			if cursor == "" {
				break
			}
			if seen[cursor] || page == 99 {
				return nil, errors.New("OpenCode 历史分页未完整读取，请指定原对话")
			}
			seen[cursor] = true
		}
	}
	out := []protocol.NativeConversation{}
	for _, s := range sessions {
		dir, err := filepath.EvalSymlinks(s.Location.Directory)
		if err != nil || !sameDirectory(expected, dir) || s.ParentID != "" || s.Time.Archived != 0 || !nativeIDPattern.MatchString(s.ID) {
			continue
		}
		out = append(out, protocol.NativeConversation{ID: s.ID, Title: shortTitle(s.Title), CreatedAt: s.Time.Created, UpdatedAt: s.Time.Updated})
	}
	return out, nil
}

func (a *Agent) nativeCommandHistory(c ResolvedCommand, cwd string, env []string, key, target string) ([]protocol.NativeConversation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	v2, err := isOpenCodeV2(ctx, c, env, cwd)
	if err != nil {
		return nil, err
	}
	if v2 {
		client, err := connectOpenCodeV2(ctx, c, env, cwd)
		if err != nil {
			return nil, err
		}
		return client.history(ctx, cwd, target)
	}
	return a.nativeHistory(nativeTool(c), cwd, env, key, target)
}

func (c *openCodeV2Client) prepare(ctx context.Context, binding *protocol.ConversationBinding) error {
	if binding.NativeID != "" {
		_, err := c.session(ctx, binding.NativeID)
		return err
	}
	var response struct {
		Data openCodeV2Session `json:"data"`
	}
	// Let OpenCode issue its native ID. API-valid custom UUID IDs are not
	// necessarily accepted by the hosted free-model gateway.
	if err := c.call(ctx, "POST", "/api/session", map[string]any{"location": openCodeV2Location{c.cwd}}, &response); err != nil {
		return err
	}
	if !nativeIDPattern.MatchString(response.Data.ID) {
		return errors.New("OpenCode 未确认新对话身份")
	}
	binding.NativeID = response.Data.ID
	_, err := c.session(ctx, binding.NativeID)
	return err
}

func (c *openCodeV2Client) environment(ctx context.Context, id string, env []string) error {
	variables := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		switch strings.ToUpper(key) {
		case "OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD", "CODEGATE_OPENCODE_V2_URL", "CODEGATE_CONVERSATION_MARKER":
			continue
		}
		variables[key] = value
	}
	return c.call(ctx, "PUT", "/api/session/"+url.PathEscape(id)+"/environment", map[string]any{"variables": variables}, nil)
}

type openCodeV2Message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Error *struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Status   int    `json:"status"`
		Response struct {
			Body string `json:"body"`
		} `json:"response"`
	} `json:"error"`
	Retry *struct {
		At int64 `json:"at"`
	} `json:"retry"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *openCodeV2Client) readTurn(ctx context.Context, id string) (nativeThread, nativeTurn, string, error) {
	var thread nativeThread
	var turn nativeTurn
	s, err := c.session(ctx, id)
	if err != nil {
		return thread, turn, "", err
	}
	thread.ID = id
	thread.Cwd = s.Location.Directory
	thread.Status.Type = "idle"
	var users, assistants struct {
		Data []openCodeV2Message `json:"data"`
	}
	path := "/api/session/" + url.PathEscape(id)
	if err = c.call(ctx, "GET", path+"/message?type=user&limit=1&order=desc", nil, &users); err != nil {
		return thread, turn, "", err
	}
	if len(users.Data) != 1 {
		return thread, turn, "", errors.New("OpenCode 原对话尚未开始任务")
	}
	user := users.Data[0]
	if user.Type != "user" || !strings.HasPrefix(user.ID, "msg") || user.Time.Created <= 0 {
		return thread, turn, "", errors.New("OpenCode 原生输入身份无效")
	}
	turn.ID = user.ID
	var active struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err = c.call(ctx, "GET", "/api/session/active", nil, &active); err != nil {
		return thread, turn, "", err
	}
	_, running := active.Data[id]
	if running || s.Time.Idle < user.Time.Created {
		thread.Status.Type = "active"
		turn.Status = "inProgress"
	} else {
		switch s.Outcome {
		case "succeeded":
			turn.Status = "completed"
		case "failed", "interrupted":
			turn.Status = s.Outcome
		default:
			return thread, turn, "", errors.New("OpenCode 未提供可确认的执行结果")
		}
	}
	if err = c.call(ctx, "GET", path+"/message?type=assistant&limit=1&order=desc", nil, &assistants); err != nil {
		return thread, turn, "", err
	}
	text := ""
	if len(assistants.Data) > 0 && assistants.Data[0].Time.Created >= user.Time.Created {
		m := assistants.Data[0]
		for _, part := range m.Content {
			if part.Type == "text" {
				text += part.Text
			}
		}
		if m.Retry != nil {
			turn.RetryAt = m.Retry.At
		}
		if m.Error != nil {
			code := openCodeV2ErrorCode(m)
			turn.Error = &struct {
				Info json.RawMessage `json:"codexErrorInfo"`
			}{Info: json.RawMessage(quoteJSON(code))}
		}
	}
	return thread, turn, text, nil
}

func openCodeV2ErrorCode(m openCodeV2Message) string {
	if m.Error == nil {
		return ""
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(m.Error.Response.Body), &body)
	if m.Error.Status == 401 && strings.Contains(strings.ToLower(m.Error.Message+" "+m.Error.Response.Body), "expired") {
		return "authenticationExpired"
	}
	if body.Type == "FreeTierError" || body.Error.Type == "FreeTierError" || strings.Contains(m.Error.Message, "free tier can only be used from within OpenCode") {
		return "freeTierUnavailable"
	}
	lower := strings.ToLower(m.Error.Type + " " + m.Error.Response.Body)
	if strings.Contains(lower, "usage_limit") || strings.Contains(lower, "quota_exceeded") || strings.Contains(lower, "insufficient_quota") {
		return "usageLimitExceeded"
	}
	if m.Error.Status == 429 {
		return "rateLimitExceeded"
	}
	if m.Error.Status >= 500 {
		return "serverOverloaded"
	}
	return m.Error.Type
}

func nativeFailureDetail(turn nativeTurn) string {
	var code string
	if turn.Error != nil {
		_ = json.Unmarshal(turn.Error.Info, &code)
	}
	switch code {
	case "authenticationExpired":
		return "OpenCode 登录凭据已过期（401）；请在本机 OpenCode 使用 /connect 重新连接账号，再重新安排。不要把 API key 发到任务或聊天中；未切换付费模型"
	case "freeTierUnavailable":
		return "OpenCode 免费模型网关拒绝了本次请求；请先在本机 OpenCode 验证该免费模型可用，再重新安排。未切换付费模型或放宽工具权限"
	case "provider.auth":
		return "模型服务拒绝认证或访问，请在本机 CLI 检查登录与模型访问权限后重新安排；未切换模型"
	case "usageLimitExceeded":
		return "模型服务报告额度不足，请确认额度恢复后重新安排；未切换模型"
	default:
		return "原生模型执行失败，请检查本机 CLI 和原生对话；未自动重发指令"
	}
}

func (c *openCodeV2Client) prompt(ctx context.Context, id, messageID, text string) error {
	var response struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.call(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/prompt", map[string]any{"id": messageID, "text": text}, &response); err != nil {
		return err
	}
	if response.Data.ID != messageID {
		return errors.New("OpenCode 提交回执身份不一致，未重发指令")
	}
	return nil
}

func (c *openCodeV2Client) queued(ctx context.Context, id string) (bool, error) {
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	err := c.call(ctx, "GET", "/api/session/"+url.PathEscape(id)+"/inbox", nil, &response)
	return len(response.Data) > 0, err
}

func (c *openCodeV2Client) interrupt(ctx context.Context, id, expected string) error {
	_, turn, _, err := c.readTurn(ctx, id)
	if err != nil {
		return err
	}
	if turn.ID != expected {
		return errors.New("OpenCode 已切换到其他输入，未中断其他任务")
	}
	if turn.Status != "inProgress" {
		return nil
	}
	var response struct {
		Interrupted bool `json:"interrupted"`
	}
	if err = c.call(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/interrupt?resume=false", nil, &response); err != nil {
		return err
	}
	for attempt := 0; attempt < 20; attempt++ {
		_, turn, _, err = c.readTurn(ctx, id)
		if err != nil || turn.ID != expected {
			break
		}
		if turn.Status != "inProgress" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("不能确认 OpenCode 原生执行已停止，请核对原对话")
}

func (a *Agent) closeOpenCodeV2Session(source protocol.SessionSummary) error {
	if source.Recovery == nil || source.Recovery.NativeID == "" || nativeTool(ResolvedCommand{Command: source.Command, Args: source.Args}) != "opencode" {
		return nil
	}
	_, cmd, err := a.recoveryCommand(source)
	if err != nil {
		return err
	}
	env := BuildEnv(a.cfg, 80, 24)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v2, err := isOpenCodeV2(ctx, cmd, env, source.Cwd)
	if err != nil || !v2 {
		return err
	}
	client, err := connectOpenCodeV2(ctx, cmd, env, source.Cwd)
	if err != nil {
		return err
	}
	id := source.Recovery.NativeID
	if _, err = client.session(ctx, id); err != nil {
		if openCodeV2NotFound(err) {
			return nil
		}
		return err
	}
	var inbox struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := "/api/session/" + url.PathEscape(id)
	if err = client.call(ctx, "GET", path+"/inbox", nil, &inbox); err != nil {
		return err
	}
	for _, item := range inbox.Data {
		if err = client.call(ctx, "DELETE", path+"/inbox/"+url.PathEscape(item.ID), nil, nil); err != nil {
			return err
		}
	}
	var active struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err = client.call(ctx, "GET", "/api/session/active", nil, &active); err != nil {
		return err
	}
	if _, running := active.Data[id]; !running {
		return nil
	}
	_, turn, _, err := client.readTurn(ctx, id)
	if err != nil {
		return err
	}
	return client.interrupt(ctx, id, turn.ID)
}

// Hold the existing update barrier until native work is confirmed idle, even
// if the TUI exits unexpectedly. The shared service itself is never stopped.
func (a *Agent) trackOpenCodeV2Session(sess *session.Session, env []string) {
	endpoint, err := url.Parse(nativeEnvValue(env, "CODEGATE_OPENCODE_V2_URL"))
	if err != nil {
		return
	}
	client := &openCodeV2Client{endpoint: endpoint, password: nativeEnvValue(env, "OPENCODE_PASSWORD"), cwd: sess.Cwd, http: serverHTTPClient()}
	finish, err := a.beginActivity()
	if err != nil {
		return
	}
	a.quotaMu.Lock()
	parent := a.quotaCtx
	a.quotaMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		select {
		case <-sess.Done():
		case <-parent.Done():
			finish()
			return
		}
		a.releaseOpenCodeV2WhenIdle(parent, client, sess.Summary().Recovery.NativeID, finish)
	}()
}

func (a *Agent) releaseOpenCodeV2WhenIdle(parent context.Context, client *openCodeV2Client, id string, finish func()) {
	defer finish()
	for {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		var active struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		err := client.call(ctx, "GET", "/api/session/active", nil, &active)
		var inbox struct {
			Data []json.RawMessage `json:"data"`
		}
		if err == nil {
			err = client.call(ctx, "GET", "/api/session/"+url.PathEscape(id)+"/inbox", nil, &inbox)
		}
		cancel()
		_, running := active.Data[id]
		if !running && (err == nil && len(inbox.Data) == 0 || openCodeV2NotFound(err)) {
			return
		}
		select {
		case <-parent.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// An accepted prompt can still be in the durable inbox when cancellation
// arrives. Remove that exact item before trying to interrupt its execution.
func (c *openCodeV2Client) stopInput(ctx context.Context, id, own string) error {
	path := "/api/session/" + url.PathEscape(id)
	var inbox struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.call(ctx, "GET", path+"/inbox", nil, &inbox); err != nil {
		return err
	}
	removed := false
	for _, item := range inbox.Data {
		if item.ID == own {
			if err := c.call(ctx, "DELETE", path+"/inbox/"+url.PathEscape(own), nil, nil); err != nil {
				return err
			}
			removed = true
		}
	}
	_, turn, _, err := c.readTurn(ctx, id)
	if removed && (err != nil || turn.ID != own) {
		return nil
	}
	if err != nil {
		return err
	}
	if turn.ID != own {
		return errors.New("原生输入身份已变化，请核对执行结果")
	}
	return c.interrupt(ctx, id, own)
}

func (a *Agent) startOpenCodeV2Task(req *protocol.Envelope, p protocol.TaskRunPayload, receipt taskReceipt, run *backgroundRun, key string, client *openCodeV2Client, ctx context.Context, cancel context.CancelFunc, finish func()) {
	receipt.Result.State = "running"
	receipt.Result.Detail = "正在向 OpenCode 原对话提交后台任务"
	receipt.Result.Progress = &protocol.TaskProgress{Sequence: 1, StartedAt: time.Now().UnixMilli(), ObservedAt: time.Now().UnixMilli(), Activity: "正在提交原生任务"}
	a.tasksMu.Lock()
	run.receipt = receipt
	a.tasksMu.Unlock()
	a.reply(req, protocol.TypeTaskState, receipt.Result)
	go func() {
		defer a.tasksWG.Done()
		// A lost response does not prove execution stopped. Keep the update
		// barrier until native work is idle, including needs_review outcomes.
		a.quotaMu.Lock()
		parent := a.quotaCtx
		a.quotaMu.Unlock()
		if parent == nil {
			parent = context.Background()
		}
		defer func() { go a.releaseOpenCodeV2WhenIdle(parent, client, p.Source.Recovery.NativeID, finish) }()
		defer cancel()
		id := p.Source.Recovery.NativeID
		own := "msg_" + promptDigest(p.TaskID+":"+itoa(p.Attempt))
		complete := func(state, detail string) {
			receipt.Result.State = state
			receipt.Result.Detail = detail
			a.tasksMu.Lock()
			defer a.tasksMu.Unlock()
			receipt.Result.Progress = run.receipt.Result.Progress
			if receipt.Result.Progress != nil {
				cp := *receipt.Result.Progress
				cp.Sequence++
				cp.ObservedAt = time.Now().UnixMilli()
				receipt.Result.Progress = &cp
			}
			run.receipt = receipt
			a.publishTask(receipt)
			delete(a.tasks, key)
		}
		stopOwn := func() {
			stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			err := client.stopInput(stopCtx, id, own)
			stop()
			if err != nil {
				complete("needs_review", "无法确认 OpenCode 后台执行已停止，请检查原对话")
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				complete("failed", "达到运行上限，已确认原生执行停止")
			} else {
				complete("cancelled", "已确认 OpenCode 本次后台执行停止")
			}
		}
		if err := client.prompt(ctx, id, own, p.Prompt); err != nil {
			if ctx.Err() != nil {
				stopOwn()
			} else {
				complete("needs_review", "OpenCode 提交回执无法确认，请检查原对话；未重复提交")
			}
			return
		}
		if err := a.saveTaskReceipt(receipt); err != nil {
			cancel()
		}
		observed := false
		lastProgress := time.Time{}
		for {
			if ctx.Err() != nil {
				stopOwn()
				return
			}
			_, turn, text, err := client.readTurn(ctx, id)
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				complete("needs_review", "OpenCode 原生状态无法确认，请检查原对话；未重发指令")
				return
			}
			if turn.ID == own {
				observed = true
			} else if observed {
				complete("needs_review", "原对话已有其他输入，本次结果请核对；未中断其他任务")
				return
			}
			if observed && turn.Status != "inProgress" {
				receipt.Result.Output = string([]rune(text)[:min(len([]rune(text)), 4096)])
				switch turn.Status {
				case "completed":
					complete("completed", "本次 OpenCode 后台执行结束，请检查结果")
				case "interrupted":
					complete("cancelled", "原生执行已中断")
				default:
					if p.Mode == "quota" && usageWall(turn) {
						a.quotaMu.Lock()
						delete(a.quotaCache, "opencode")
						a.quotaMu.Unlock()
						checkCtx, stop := context.WithTimeout(parent, 12*time.Second)
						quota, err := a.readProviderQuota(checkCtx, "opencode")
						stop()
						if err == nil {
							if until := blockedUntil(quota, time.Now()); until > 0 {
								receipt.Result.Retry = true
								receipt.Result.DueAt = until * 1000
								complete("waiting_quota", "已确认订阅额度耗尽，恢复后继续同一 OpenCode 原生对话")
								return
							}
						}
					}
					complete("failed", nativeFailureDetail(turn))
				}
				return
			}
			if observed && time.Since(lastProgress) >= 2*time.Second {
				activity := "OpenCode 原生回合正在执行；详细工具活动暂不可用"
				activityAt := int64(0)
				a.tasksMu.Lock()
				previous := run.receipt.Result.Progress
				changed := previous == nil || previous.Text != workflowClip(text, 4096)
				a.tasksMu.Unlock()
				if changed && text != "" {
					activity = "收到模型回复"
					activityAt = time.Now().UnixMilli()
				}
				a.updateTaskProgress(run, text, activity, activityAt)
				lastProgress = time.Now()
			}
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

type openCodeV2Control struct {
	client *openCodeV2Client
	ctx    context.Context
	a      *Agent
	p      protocol.TaskRunPayload
	last   nativeTurn
	id     string
}

func (c *openCodeV2Control) close() {}
func (c *openCodeV2Control) readThread(id string) (nativeThread, nativeTurn, error) {
	thread, turn, _, err := c.client.readTurn(c.ctx, id)
	if err == nil {
		c.last = turn
		c.id = id
	}
	return thread, turn, err
}
func (c *openCodeV2Control) usageAllowed() (bool, error) {
	s, err := c.client.session(c.ctx, c.id)
	if err != nil {
		return false, err
	}
	snapshot := liveEventSnapshot{At: s.Time.Idle}
	snapshot.Model.ProviderID = s.Model.ProviderID
	control := &eventControl{a: c.a, ctx: c.ctx, tool: "opencode", last: snapshot, p: c.p}
	return control.usageAllowed()
}
func (c *openCodeV2Control) call(method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ClientID string `json:"clientUserMessageId"`
		Input    []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.ThreadID != c.id || c.id == "" {
		return errors.New("OpenCode 原生控制目标不一致")
	}
	if method == "turn/interrupt" {
		if p.TurnID != c.last.ID {
			return errors.New("OpenCode 原生回合身份已变化")
		}
		if err = c.client.interrupt(c.ctx, p.ThreadID, p.TurnID); err != nil {
			return err
		}
		return json.Unmarshal([]byte(`{}`), out)
	}
	if method != "turn/start" || len(p.Input) != 1 || p.ClientID == "" {
		return errors.New("无效的 OpenCode 原生提交")
	}
	_, latest, _, err := c.client.readTurn(c.ctx, c.id)
	if err != nil {
		return err
	}
	if latest.ID != c.last.ID || latest.Status == "inProgress" {
		return errors.New("OpenCode 原生状态已变化，未提交")
	}
	queued, err := c.client.queued(c.ctx, c.id)
	if err != nil {
		return err
	}
	if queued {
		return errors.New("OpenCode 原对话已有待处理输入，未自动提交")
	}
	id := "msg_" + promptDigest(p.ClientID)
	if err = c.client.prompt(c.ctx, c.id, id, p.Input[0].Text); err != nil {
		return err
	}
	result := map[string]any{"turn": map[string]any{"id": id, "status": "inProgress"}}
	raw, _ = json.Marshal(result)
	return json.Unmarshal(raw, out)
}
