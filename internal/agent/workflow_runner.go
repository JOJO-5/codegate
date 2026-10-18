package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type workflowOutput struct {
	mu        sync.Mutex
	line      []byte
	text, id  string
	record    func(string) error
	recordErr error
}

func (o *workflowOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			o.consume()
			o.line = nil
		} else if len(o.line) < 1<<20 {
			o.line = append(o.line, b)
		}
	}
	return len(p), nil
}
func (o *workflowOutput) consume() {
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(o.line, &event) != nil {
		return
	}
	if event.Type == "thread.started" {
		if o.id != event.ThreadID && event.ThreadID != "" {
			o.id = event.ThreadID
			if o.record != nil {
				o.recordErr = o.record(o.id)
			}
		}
	}
	if event.Type == "item.completed" && event.Item.Type == "agent_message" {
		o.text = workflowClip(event.Item.Text, 16000)
	}
}
func (a *Agent) workflowTurn(ctx context.Context, p protocol.WorkflowProfile, cwd, nativeID, prompt string, master bool, record func(string) error) (workflowTurnResult, error) {
	if err := ctx.Err(); err != nil {
		return workflowTurnResult{}, err
	}
	c, err := a.workflowCommand(p)
	if err != nil {
		return workflowTurnResult{}, err
	}
	env := BuildEnv(a.cfg, 80, 24)
	if p.Tool == "opencode" {
		return a.workflowOpenCodeTurn(ctx, c, p, env, cwd, nativeID, prompt, master, record)
	}
	if p.Tool != "codex" {
		return workflowTurnResult{}, errors.New("此 CLI 暂不支持主 Agent 调度")
	}
	args := append([]string{}, c.Args...)
	for _, arg := range args {
		switch arg {
		case "exec", "resume", "run", "--last", "--ephemeral":
			return workflowTurnResult{}, errors.New("调度需要普通 CLI 配置，不能包含预设会话或执行模式")
		}
	}
	prefix := []string{}
	if strings.EqualFold(strings.TrimSuffix(filepath.Base(c.Command), ".exe"), "cmd") && len(args) >= 2 && strings.EqualFold(args[0], "/c") {
		prefix = append(prefix, args[:2]...)
		args = args[2:]
	}
	filtered := []string{}
	for _, arg := range args {
		if arg != "--no-alt-screen" {
			filtered = append(filtered, arg)
		}
	}
	args = append(prefix, filtered...)
	// The owned standalone process can be stopped without touching the shared
	// daemon that hosts the user's other Codex sessions.
	args = append(args, "--no-daemon", "exec")
	if nativeID != "" {
		if !nativeIDPattern.MatchString(nativeID) {
			return workflowTurnResult{}, errors.New("无效原生主对话身份")
		}
		args = append(args, "resume", nativeID)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	if nativeID == "" {
		if master {
			args = append(args, "--sandbox", "read-only")
		} else {
			args = append(args, "--sandbox", "workspace-write")
		}
	}
	args = append(args, "-")
	cmd := exec.Command(c.Command, args...)
	processutil.Background(cmd)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	output := &workflowOutput{record: record}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		return workflowTurnResult{}, errors.New("Codex 调度进程启动失败，请检查 CLI 版本和本机配置")
	}
	stop := context.AfterFunc(ctx, func() { _ = tree.Stop() })
	err = cmd.Wait()
	stop()
	_ = tree.Stop()
	output.mu.Lock()
	output.consume()
	text, id, recordErr := output.text, output.id, output.recordErr
	output.mu.Unlock()
	if nativeID != "" {
		id = nativeID
	}
	result := workflowTurnResult{ID: id, Text: text, State: "completed"}
	if recordErr != nil {
		return result, errors.New("无法保存原生对话身份，请核对执行结果")
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		result.State = "failed"
		result.Text = "CLI 执行失败，请核对原生对话"
		return result, nil
	}
	if text == "" || id == "" {
		return result, errors.New("CLI 未提供可核对的最终输出或原生对话身份")
	}
	return result, nil
}

func (a *Agent) workflowOpenCodeTurn(ctx context.Context, c ResolvedCommand, p protocol.WorkflowProfile, env []string, cwd, nativeID, prompt string, master bool, record func(string) error) (workflowTurnResult, error) {
	client, err := connectOpenCodeV2(ctx, c, env, cwd)
	if err != nil {
		return workflowTurnResult{}, err
	}
	models, err := workflowModels(ctx, client)
	if err != nil {
		return workflowTurnResult{}, err
	}
	found := false
	for _, m := range models {
		if m.ID == p.Model && m.Provider == p.Provider && m.Status == "active" && m.Capabilities.Tools {
			if p.Free && !m.free() {
				return workflowTurnResult{}, errors.New("免费模型价格变动或未知，已停止派发，没有转用付费模型")
			}
			found = true
			break
		}
	}
	if !found {
		return workflowTurnResult{}, errors.New("指定模型当前不可用，未更换模型")
	}
	binding := &protocol.ConversationBinding{NativeID: nativeID}
	if err = client.prepare(ctx, binding); err != nil {
		return workflowTurnResult{}, err
	}
	nativeID = binding.NativeID
	// Only tighten this owned session's tool permissions. A wildcard deny also
	// blocks V2 provider authentication, even for hosted free models. Workers
	// edit files; shell verification stays in the bounded host check adapter.
	if err = client.call(ctx, "PATCH", "/api/session/"+nativeID, map[string]any{"permissions": workflowOpenCodePermissions(master)}, nil); err != nil {
		return workflowTurnResult{}, err
	}
	if err = client.environment(ctx, nativeID, env); err != nil {
		return workflowTurnResult{}, err
	}
	if err = client.call(ctx, "POST", "/api/session/"+nativeID+"/model", map[string]any{"model": map[string]string{"providerID": p.Provider, "id": p.Model}}, nil); err != nil {
		return workflowTurnResult{}, err
	}
	if record != nil {
		if err := record(nativeID); err != nil {
			return workflowTurnResult{ID: nativeID}, err
		}
	}
	own := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	result := workflowTurnResult{ID: nativeID}
	stopNative := func() error {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		e := client.stopInput(cleanup, nativeID, own)
		if e != nil {
			finish, activityErr := a.beginActivity()
			if activityErr == nil {
				a.quotaMu.Lock()
				parent := a.quotaCtx
				a.quotaMu.Unlock()
				if parent == nil {
					parent = context.Background()
				}
				go a.releaseOpenCodeV2WhenIdle(parent, client, nativeID, finish)
			}
			return errors.New("不能确认原生模型执行已停止，请核对原生对话")
		}
		return nil
	}
	if err = client.prompt(ctx, nativeID, own, prompt); err != nil {
		if ctx.Err() != nil {
			if e := stopNative(); e != nil {
				return result, e
			}
			return result, ctx.Err()
		}
		_ = stopNative()
		return result, errors.New("模型提交结果无法确认，未重复提交")
	}
	observed := false
	for {
		if ctx.Err() != nil {
			if e := stopNative(); e != nil {
				return result, e
			}
			return result, ctx.Err()
		}
		_, turn, text, e := client.readTurn(ctx, nativeID)
		if e != nil {
			if ctx.Err() != nil {
				continue
			}
			if queued, queueErr := client.queued(ctx, nativeID); queueErr == nil && queued && !observed {
				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			_ = stopNative()
			return result, errors.New("原生模型状态读取失败，请核对执行结果")
		}
		if turn.ID == own {
			observed = true
		} else if observed {
			_ = stopNative()
			return result, errors.New("原生对话出现其他输入，未中断其他任务")
		}
		if turn.Status == "inProgress" {
			var permissions struct {
				Data []json.RawMessage `json:"data"`
			}
			if e := client.call(ctx, "GET", "/api/session/"+nativeID+"/permission", nil, &permissions); e == nil && len(permissions.Data) > 0 {
				_ = stopNative()
				return result, errors.New("工作 CLI 等待本机权限确认，已暂停调度；请核对原生对话和本机授权")
			}
		}
		if observed && turn.Status != "inProgress" {
			result.Text = text
			result.State = turn.Status
			if result.State == "failed" {
				result.Text = nativeFailureDetail(turn)
				var code string
				if turn.Error != nil {
					_ = json.Unmarshal(turn.Error.Info, &code)
				}
				if code == "authenticationExpired" || code == "provider.auth" || code == "freeTierUnavailable" {
					result.State = "needs_review"
				}
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func workflowOpenCodePermissions(master bool) []map[string]string {
	actions := []string{"shell", "subagent", "external_directory", "question"}
	if master {
		actions = append(actions, "edit", "execute", "skill")
	}
	rules := make([]map[string]string, 0, len(actions))
	for _, action := range actions {
		rules = append(rules, map[string]string{"action": action, "resource": "*", "effect": "deny"})
	}
	return rules
}

type workflowCheckOutput struct {
	mu   sync.Mutex
	data []byte
}

func (o *workflowCheckOutput) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(b)
	if len(o.data) < 8000 {
		take := min(n, 8000-len(o.data))
		o.data = append(o.data, b[:take]...)
	}
	return n, nil
}
func workflowCheck(ctx context.Context, cwd, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command := ""
	args := []string{}
	switch id {
	case "git_diff_check":
		command = "git"
		args = []string{"diff", "--check", "HEAD"}
	case "node_test":
		command = "node"
		args = []string{"--test"}
	case "go_test":
		command = "go"
		args = []string{"test", "./..."}
	case "npm_test":
		command = "npm"
		args = []string{"test"}
	case "npm_build":
		command = "npm"
		args = []string{"run", "build"}
	default:
		return "", errors.New("未授权验证入口")
	}
	// Resolve npm's Windows shim through the existing CLI normalization.
	resolved, resolveErr := exec.LookPath(command)
	if resolveErr != nil {
		return "本机未安装验证所需工具：" + command, resolveErr
	}
	if runtime.GOOS == "windows" && (strings.HasSuffix(strings.ToLower(resolved), ".cmd") || strings.HasSuffix(strings.ToLower(resolved), ".bat")) {
		args = append([]string{"/c", resolved}, args...)
		resolved = "cmd.exe"
	}
	cmd := exec.Command(resolved, args...)
	processutil.Background(cmd)
	cmd.Dir = cwd
	cmd.Env = BuildEnv(Config{}, 80, 24)
	output := &workflowCheckOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	child, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		return "无法启动验证，请检查本机工具安装", err
	}
	stop := context.AfterFunc(child, func() { _ = tree.Stop() })
	err = cmd.Wait()
	stop()
	_ = tree.Stop()
	if child.Err() != nil {
		return string(output.data), child.Err()
	}
	return string(output.data), err
}
func workflowFiles(ctx context.Context, cwd string, expectedHead ...string) (workflowArtifact, error) {
	var result workflowArtifact
	head, headCut, err := runGit(ctx, cwd, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if headCut || len(expectedHead) > 0 && strings.TrimSpace(head) != expectedHead[0] {
		return result, errors.New("工作分支的提交已变化，请核对实际产物；调度未继续")
	}
	// Only the workflow's independent index is touched. Intent-to-add makes new
	// source files visible in the diff without creating a commit.
	if _, _, err := runGit(ctx, cwd, "add", "-N", "--", "."); err != nil {
		return result, err
	}
	raw, cut, err := runGit(ctx, cwd, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--", ".")
	if err != nil {
		return result, err
	}
	status, statusCut, err := runGit(ctx, cwd, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return result, err
	}
	result.Fingerprint = promptDigest(head + "\n" + raw + "\n" + status)
	result.Diff = workflowClip(raw, 6000)
	result.Truncated = cut || statusCut || len([]rune(raw)) > 6000
	return result, nil
}
