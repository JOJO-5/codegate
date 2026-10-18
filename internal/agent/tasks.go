package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type taskReceipt struct {
	Live   *liveQuotaReceipt         `json:"live,omitempty"`
	UserID string                    `json:"user_id"`
	Hash   string                    `json:"hash"`
	Source protocol.SessionSummary   `json:"source"`
	Result protocol.TaskStatePayload `json:"result"`
}
type backgroundRun struct {
	receipt taskReceipt
	cancel  context.CancelFunc
}

func taskRunKey(id string, attempt int) string { return id + "-" + itoa(attempt) }
func itoa(n int) string                        { return strconv.Itoa(n) }
func continuationHash(p protocol.TaskRunPayload) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (a *Agent) receiptPath(id string, attempt int) (string, error) {
	if _, e := uuid.Parse(id); e != nil || attempt < 1 || attempt > 11 {
		return "", errors.New("invalid task identity")
	}
	return filepath.Join(a.cfg.StateDir, "task-receipts", taskRunKey(id, attempt)+".json"), nil
}
func (a *Agent) saveTaskReceipt(r taskReceipt) error {
	path, err := a.receiptPath(r.Result.TaskID, r.Result.Attempt)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp, path)
}
func (a *Agent) taskReceipt(id string, attempt int, user string) (taskReceipt, error) {
	path, err := a.receiptPath(id, attempt)
	if err != nil {
		return taskReceipt{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return taskReceipt{}, err
	}
	var r taskReceipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.UserID != user {
		return r, errors.New("task owner mismatch")
	}
	return r, nil
}
func sameNativeTask(a, b protocol.SessionSummary) bool {
	return a.Recovery != nil && b.Recovery != nil && a.Recovery.NativeID != "" && a.Recovery.NativeID == b.Recovery.NativeID && quotaProvider(a.Command, a.Args) == quotaProvider(b.Command, b.Args) && a.Recovery.StoreKey == b.Recovery.StoreKey && a.Recovery.NativeRoot == b.Recovery.NativeRoot
}
func (a *Agent) nativeTaskBusy(source protocol.SessionSummary) bool {
	a.tasksMu.Lock()
	defer a.tasksMu.Unlock()
	for _, run := range a.tasks {
		if sameNativeTask(source, run.receipt.Source) {
			return true
		}
	}
	return false
}
func (a *Agent) onTaskInspect(req *protocol.Envelope, cancel bool) {
	p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
	if err != nil {
		a.replyError(req, err)
		return
	}
	if p.Mode == "live_quota" {
		a.onLiveQuota(req, p, cancel, true)
		return
	}
	key := taskRunKey(p.TaskID, p.Attempt)
	a.tasksMu.Lock()
	run := a.tasks[key]
	if run != nil && run.receipt.UserID == p.UserID {
		result := run.receipt.Result
		if cancel {
			run.cancel()
			result.State = "cancelling"
		}
		a.tasksMu.Unlock()
		a.reply(req, protocol.TypeTaskState, result)
		return
	}
	a.tasksMu.Unlock()
	r, err := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if err != nil {
		a.reply(req, protocol.TypeTaskState, protocol.TaskStatePayload{TaskID: p.TaskID, Attempt: p.Attempt, State: "needs_review", Detail: "没有可核对的执行回执，请确认原生历史后再安排"})
		return
	}
	if r.Result.State == "running" || r.Result.State == "preparing" {
		r.Result.State = "needs_review"
		r.Result.Detail = "Agent重启后无法确认上次执行结果，请检查原生对话"
	}
	a.reply(req, protocol.TypeTaskState, r.Result)
}
func (a *Agent) publishTask(r taskReceipt) {
	if err := a.saveTaskReceipt(r); err != nil {
		r.Result.State = "needs_review"
		r.Result.Detail = "执行结束但不能保存回执，请检查原生历史"
	}
	env, err := protocol.NewEnvelope(protocol.TypeTaskState, r.Result)
	if err == nil {
		_ = a.sendControl(env)
	}
}
func (a *Agent) onTaskRun(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
	result := protocol.TaskStatePayload{TaskID: p.TaskID, Attempt: p.Attempt, State: "failed"}
	fail := func(message string) { result.Detail = message; a.reply(req, protocol.TypeTaskState, result) }
	if err != nil || p.UserID == "" || p.Source.Recovery == nil || !nativeIDPattern.MatchString(p.Source.Recovery.NativeID) || len(p.Prompt) == 0 || len(p.Prompt) > 16<<10 || p.MaxRunMinutes < 1 || p.MaxRunMinutes > 360 {
		fail("续跑配置或原生对话身份无效")
		return
	}
	if p.Mode == "live_quota" {
		a.onLiveQuota(req, p, false, false)
		return
	}
	key := taskRunKey(p.TaskID, p.Attempt)
	if _, err = a.receiptPath(p.TaskID, p.Attempt); err != nil {
		fail("任务身份无效")
		return
	}
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	if existing, e := a.taskReceipt(p.TaskID, p.Attempt, p.UserID); e == nil {
		if existing.Hash != continuationHash(p) {
			fail("同一执行标识的任务内容不一致")
			return
		}
		a.onTaskInspect(req, false)
		return
	} else if !os.IsNotExist(e) {
		fail("不能安全读取执行回执")
		return
	}
	liveCount := 0
	for _, s := range a.mgr.Snapshot() {
		if isRecoverableLive(s) {
			liveCount++
		}
		if isRecoverableLive(s) && (s.SessionID == p.Source.SessionID || sameNativeTask(s, p.Source)) {
			result.State = "waiting_idle"
			result.DueAt = time.Now().Add(time.Minute).UnixMilli()
			result.Detail = "原对话仍有终端运行，请先结束进程"
			a.reply(req, protocol.TypeTaskState, result)
			return
		}
	}
	a.tasksMu.Lock()
	activeCount := len(a.tasks)
	a.tasksMu.Unlock()
	if a.cfg.MaxSessions > 0 && activeCount+liveCount >= a.cfg.MaxSessions {
		result.State = "waiting_idle"
		result.DueAt = time.Now().Add(time.Minute).UnixMilli()
		result.Detail = "等待本机并发会话空位"
		a.reply(req, protocol.TypeTaskState, result)
		return
	}
	if a.nativeTaskBusy(p.Source) {
		result.State = "waiting_idle"
		result.DueAt = time.Now().Add(time.Minute).UnixMilli()
		result.Detail = "同一原生对话已有后台任务"
		a.reply(req, protocol.TypeTaskState, result)
		return
	}
	_, c, err := a.recoveryCommand(p.Source)
	if err != nil {
		fail("原CLI配置未获授权或已经变更")
		return
	}
	cwd, err := a.ws.Resolve(p.Source.Cwd)
	if err != nil {
		fail("工作目录已不在授权范围")
		return
	}
	env := BuildEnv(a.cfg, 80, 24)
	history, err := a.nativeCommandHistory(c, cwd, env, p.Source.Recovery.StoreKey, p.Source.Recovery.NativeID)
	if err != nil || len(history) != 1 || history[0].ID != p.Source.Recovery.NativeID {
		fail("原生对话不存在，不能创建新对话代替续跑")
		return
	}
	var openCode *openCodeV2Client
	if nativeTool(c) == "opencode" {
		checkCtx, stopCheck := context.WithTimeout(context.Background(), 30*time.Second)
		v2, checkErr := isOpenCodeV2(checkCtx, c, env, cwd)
		if checkErr == nil && v2 {
			openCode, checkErr = connectOpenCodeV2(checkCtx, c, env, cwd)
		}
		if checkErr == nil && openCode != nil {
			_, turn, _, e := openCode.readTurn(checkCtx, p.Source.Recovery.NativeID)
			queued := false
			if e == nil {
				queued, e = openCode.queued(checkCtx, p.Source.Recovery.NativeID)
			}
			checkErr = e
			if e == nil && (turn.Status == "inProgress" || queued) {
				stopCheck()
				result.State = "waiting_idle"
				result.DueAt = time.Now().Add(time.Minute).UnixMilli()
				result.Detail = "OpenCode 原对话仍在后台执行，等待空闲"
				a.reply(req, protocol.TypeTaskState, result)
				return
			}
		}
		stopCheck()
		if checkErr != nil {
			fail("无法核对 OpenCode 后台执行状态，未提交指令")
			return
		}
	}
	if p.Mode == "quota" {
		if !continuationQuotaEligible(c) {
			fail("此CLI配置无法可靠确认订阅额度，请选择按时间继续")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		q, e := a.readProviderQuota(ctx, quotaProvider(c.Command, c.Args))
		cancel()
		if e != nil || len(q.Providers) != 1 || q.Providers[0].Status != "ok" {
			fail("额度暂不可查询，未启动模型；可改为明确的按时间继续")
			return
		}
		blocked := blockedUntil(q, time.Now())
		if blocked > 0 {
			result.State = "waiting_quota"
			result.DueAt = blocked * 1000
			result.Detail = "额度仍耗尽，等待所有已知阻塞窗口恢复"
			a.reply(req, protocol.TypeTaskState, result)
			return
		}
	}
	args, err := continuationArgs(c, p.Source.Recovery.NativeID)
	if err != nil {
		fail(err.Error())
		return
	}
	finish, err := a.beginActivity()
	if err != nil {
		result.State = "waiting_agent"
		result.DueAt = time.Now().Add(time.Minute).UnixMilli()
		result.Detail = "Agent正在更新"
		a.reply(req, protocol.TypeTaskState, result)
		return
	}
	a.quotaMu.Lock()
	parent := a.quotaCtx
	a.quotaMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.MaxRunMinutes)*time.Minute)

	result.State = "preparing"
	receipt := taskReceipt{UserID: p.UserID, Hash: continuationHash(p), Source: p.Source, Result: result}
	a.tasksMu.Lock()
	if a.tasksClosing {
		a.tasksMu.Unlock()
		cancel()
		finish()
		fail("Agent正在退出")
		return
	}
	if a.tasks == nil {
		a.tasks = make(map[string]*backgroundRun)
	}
	run := &backgroundRun{receipt: receipt, cancel: cancel}
	a.tasks[key] = run
	a.tasksWG.Add(1)
	a.tasksMu.Unlock()
	// The durable prepare marker is written before spawning or feeding the CLI.
	if err = a.saveTaskReceipt(receipt); err != nil {
		a.tasksMu.Lock()
		delete(a.tasks, key)
		a.tasksMu.Unlock()
		a.tasksWG.Done()
		cancel()
		finish()
		fail("不能保存幂等回执，未启动模型")
		return
	}
	if openCode != nil {
		a.startOpenCodeV2Task(req, p, receipt, run, key, openCode, ctx, cancel, finish)
		return
	}
	cmd := exec.Command(c.Command, args...)
	processutil.Background(cmd)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = strings.NewReader(p.Prompt + "\n")
	output := &taskOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		receipt.Result.State = "failed"
		receipt.Result.Detail = "CLI后台启动失败，请更新CLI或检查本机配置"
		a.publishTask(receipt)
		a.tasksMu.Lock()
		delete(a.tasks, key)
		a.tasksMu.Unlock()
		a.tasksWG.Done()
		cancel()
		finish()
		a.reply(req, protocol.TypeTaskState, receipt.Result)
		return
	}
	receipt.Result.State = "running"
	receipt.Result.Progress = &protocol.TaskProgress{Sequence: 1, StartedAt: time.Now().UnixMilli(), ObservedAt: time.Now().UnixMilli(), Activity: "CLI 已启动，等待模型活动"}
	a.tasksMu.Lock()
	run.receipt = receipt
	a.tasksMu.Unlock()
	if err := a.saveTaskReceipt(receipt); err != nil {
		cancel()
	}
	a.reply(req, protocol.TypeTaskState, receipt.Result)
	go func() {
		defer a.tasksWG.Done()
		defer finish()
		defer cancel()
		stopped := make(chan struct{})
		progressDone := make(chan struct{})
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopped:
					return
				case <-ticker.C:
					text, activity, at := output.Snapshot()
					a.updateTaskProgress(run, text, activity, at)
				}
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				_ = tree.Stop()
			case <-stopped:
			}
		}()
		err := cmd.Wait()
		close(stopped)
		<-progressDone
		_ = tree.Stop()
		receipt.Result.Output = output.Text()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.Result.State = "failed"
			receipt.Result.Detail = "达到本次运行时间上限，进程已停止，请检查原对话"
		} else if ctx.Err() != nil {
			receipt.Result.State = "cancelled"
			receipt.Result.Detail = "已取消或达到运行时间上限"
		} else if output.EnvironmentBlocked() {
			receipt.Result.State = "needs_review"
			receipt.Result.Detail = "CLI 工具报告 Codex 沙箱初始化失败，尚未确认恢复；请检查本机执行环境后重新安排"
		} else if err != nil {
			receipt.Result.State = "failed"
			receipt.Result.Detail = "CLI执行失败，请查看原生对话；未自动重发指令"
			if p.Mode == "quota" {
				provider := quotaProvider(c.Command, c.Args)
				a.quotaMu.Lock()
				delete(a.quotaCache, provider)
				a.quotaMu.Unlock()
				checkCtx, stopCheck := context.WithTimeout(parent, 12*time.Second)
				q, e := a.readProviderQuota(checkCtx, provider)
				stopCheck()
				if e == nil {
					if until := blockedUntil(q, time.Now()); until > 0 {
						receipt.Result.State = "waiting_quota"
						receipt.Result.Retry = true
						receipt.Result.DueAt = until * 1000
						receipt.Result.Detail = "已确认订阅额度耗尽，恢复后继续同一原生对话"
					}
				}
			}
		} else {
			receipt.Result.State = "completed"
			receipt.Result.Detail = "本次后台执行结束；请检查结果"
		}
		a.tasksMu.Lock()
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
		a.tasksMu.Unlock()
	}()
}

func (a *Agent) updateTaskProgress(run *backgroundRun, text, activity string, at int64) {
	a.tasksMu.Lock()
	defer a.tasksMu.Unlock()
	previous := run.receipt.Result.Progress
	p := protocol.TaskProgress{StartedAt: time.Now().UnixMilli()}
	if previous != nil {
		p = *previous
	}
	p.Sequence++
	p.ObservedAt = time.Now().UnixMilli()
	if activity != "" {
		p.Activity = activity
		p.ActivityAt = at
	}
	p.Text = workflowClip(text, 4096)
	run.receipt.Result.Progress = &p
	// A transient snapshot write failure must not declare a running process
	// finished or remove it from cancellation/inspection tracking.
	if err := a.saveTaskReceipt(run.receipt); err != nil {
		return
	}
	if env, err := protocol.NewEnvelope(protocol.TypeTaskState, run.receipt.Result); err == nil {
		_ = a.sendControl(env)
	}
}
func (a *Agent) stopBackgroundTasks() {
	a.tasksMu.Lock()
	a.tasksClosing = true
	for _, run := range a.tasks {
		run.cancel()
	}
	for _, run := range a.liveRuns {
		run.cancel()
	}
	for _, run := range a.workflowRuns {
		run.cancel()
	}
	a.tasksMu.Unlock()
	a.tasksWG.Wait()
}

// Keep only model text, never tool/debug events or stderr containing credentials.
type taskOutput struct {
	mu                 sync.Mutex
	line               []byte
	text               string
	environmentBlocked bool
	activity           string
	activityAt         int64
}

func (o *taskOutput) Write(p []byte) (int, error) {
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
func (o *taskOutput) consume() {
	var v struct {
		Type   string `json:"type"`
		Result string `json:"result"`
		Item   struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ExitCode *int   `json:"exit_code"`
			Output   string `json:"aggregated_output"`
		} `json:"item"`
		Part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
	}
	if json.Unmarshal(o.line, &v) != nil {
		return
	}
	activity := ""
	if v.Type == "item.started" || v.Type == "item.completed" {
		switch v.Item.Type {
		case "command_execution":
			activity = "正在执行命令"
		case "file_change":
			activity = "正在修改文件"
		case "mcp_tool_call", "web_search":
			activity = "正在调用工具"
		}
		if activity != "" && v.Type == "item.completed" {
			activity = "工具执行已返回"
		}
	}
	// Only structured failed command results report execution infrastructure
	// failure. Ordinary model text and failing application tests cannot set it.
	if v.Type == "item.completed" && v.Item.Type == "command_execution" && v.Item.ExitCode != nil {
		if *v.Item.ExitCode == 0 {
			o.environmentBlocked = false
		} else if strings.Contains(v.Item.Output, "helper_unknown_error") && strings.Contains(v.Item.Output, "setup refresh had errors") {
			o.environmentBlocked = true
		}
	}
	text := ""
	if v.Type == "item.completed" && v.Item.Type == "agent_message" {
		text = v.Item.Text
	}
	if v.Type == "text" {
		text = v.Part.Text
	}
	if v.Type == "result" {
		text = v.Result
	}
	if text != "" {
		activity = "收到模型回复"
		if len(text) > 16<<10 {
			text = text[:16<<10]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		o.text = text
	}
	if activity != "" {
		o.activity = activity
		o.activityAt = time.Now().UnixMilli()
	}
}

// Snapshot does not consume an incomplete JSON line while the CLI is writing.
func (o *taskOutput) Snapshot() (string, string, int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text, o.activity, o.activityAt
}
func (o *taskOutput) Text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.line) > 0 {
		o.consume()
		o.line = nil
	}
	return o.text
}

func (o *taskOutput) EnvironmentBlocked() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.line) > 0 {
		o.consume()
		o.line = nil
	}
	return o.environmentBlocked
}

// Unknown or unavailable usage is never interpreted as permission to retry.
func blockedUntil(q protocol.QuotaResult, now time.Time) int64 {
	if len(q.Providers) != 1 || q.Providers[0].Status != "ok" {
		return 0
	}
	until := int64(0)
	for _, w := range q.Providers[0].Windows {
		if w.UsedPercent >= 100 {
			if w.ResetsAt <= now.Unix() {
				until = max(until, now.Add(time.Minute).Unix())
			} else {
				until = max(until, w.ResetsAt+30)
			}
		}
	}
	return until
}

// Managed worktree cleanup must see background processes as well as PTYs.
// The caller holds workspaceMu, so a run cannot start between this snapshot
// and Git deciding whether it is safe to remove the directory.
func (a *Agent) backgroundWorkspaceSessions() []protocol.SessionSummary {
	a.tasksMu.Lock()
	defer a.tasksMu.Unlock()
	out := make([]protocol.SessionSummary, 0, len(a.tasks))
	for _, run := range a.tasks {
		source := run.receipt.Source
		source.Status = "running"
		out = append(out, source)
	}
	for _, run := range a.workflowRuns {
		w := run.receipt.Workflow
		if w.Worktree != "" {
			out = append(out, protocol.SessionSummary{SessionID: w.ID, DeviceID: w.DeviceID, Cwd: w.Worktree, Command: "workflow", Status: "running"})
		}
	}
	return out
}
