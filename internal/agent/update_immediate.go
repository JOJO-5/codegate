package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

func (a *Agent) applyImmediateRestart(parent context.Context) {
	a.updateIntentMu.Lock()
	intent := a.immediateRestart
	if intent == nil || !a.immediateRequested.Load() {
		a.updateIntentMu.Unlock()
		return
	}
	// Cancellation is possible while downloading, but not after closing work.
	a.activityMu.Lock()
	a.updateDraining = true
	a.activityMu.Unlock()
	a.updateIntentMu.Unlock()
	a.setUpdateStatus("draining", a.stagedVersion, "正在停止本设备的任务、关闭终端和 DSH Web；已保存的代码和对话保留")
	_ = a.flushImmediateState()
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	err := a.drainForImmediateRestart(ctx, intent.OwnerID)
	if err == nil {
		err = a.OnImmediateRestart(a.stagedPath, a.stagedVersion)
	}
	if err != nil {
		a.setUpdateStatus("error", a.stagedVersion, "立即重启未完成："+err.Error()+"；已停止的任务不会自动重放，请核对本机状态后重试")
	} else {
		a.restartRequested.Store(false)
		a.setUpdateStatus("switching", a.stagedVersion, "任务已停止，正在重启；等待新 Agent 上线确认")
	}
	a.updateIntentMu.Lock()
	a.immediateRestart = nil
	a.immediateRequested.Store(false)
	a.updateIntentMu.Unlock()
}

// Freeze admissions before cancelling anything. Work contexts and task receipts
// are local to this Agent; never terminate a shared service or an external task.
func (a *Agent) drainForImmediateRestart(ctx context.Context, owner string) error {
	a.activityMu.Lock()
	a.updateDraining = true
	a.activityMu.Unlock()
	a.mgr.Freeze()
	// Fence a live continuation already admitted before closing began. Later
	// requests fail beginActivity; watchdogs remain free to finish cancellation.
	for !a.liveQuotaMu.TryLock() {
		select {
		case <-ctx.Done():
			return errors.New("自动续跑尚未退出，未继续重启")
		case <-time.After(10 * time.Millisecond):
		}
	}
	a.liveQuotaMu.Unlock()
	receipts, err := a.immediateTaskReceipts(owner)
	if err != nil {
		return err
	}
	var sources []protocol.SessionSummary
	for _, source := range a.mgr.Snapshot() {
		if source.Status == "running" || source.Status == "detached" {
			sources = append(sources, source)
		}
	}
	_ = a.operationContext()
	a.activityMu.Lock()
	a.operationCancel()
	a.activityMu.Unlock()
	a.tasksMu.Lock()
	a.tasksClosing = true
	var workflows []*workflowRun
	for _, run := range a.tasks {
		run.cancel()
	}
	for _, run := range a.liveRuns {
		run.cancel()
	}
	for _, run := range a.workflowRuns {
		workflows = append(workflows, run)
		run.cancel()
	}
	a.tasksMu.Unlock()
	seen := map[string]bool{}
	for _, source := range sources {
		key := source.SessionID
		if source.Recovery != nil {
			key = quotaProvider(source.Command, source.Args) + ":" + source.Recovery.NativeID + ":" + source.Recovery.StoreKey + ":" + source.Recovery.NativeRoot
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := a.stopNativeForImmediateRestart(ctx, source); err != nil {
			return err
		}
	}
	for _, r := range receipts {
		if err := a.stopOwnedReceiptForRestart(ctx, r); err != nil {
			return err
		}
	}
	a.closeUploads()
	closed := make(chan error, 1)
	go func() {
		err := a.stopWeb()
		if n := a.mgr.CloseForRestart("immediate_agent_restart"); n > 0 {
			err = errors.Join(err, fmt.Errorf("%d 个终端未确认关闭", n))
		}
		a.tasksWG.Wait()
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return errors.New("等待任务停止超时，未继续重启")
	}
	for {
		if n, _ := a.activityState(); n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("仍有操作未停止，未继续重启")
		case <-time.After(10 * time.Millisecond):
		}
	}
	a.tasksMu.Lock()
	workflowUnknown := false
	for _, run := range workflows {
		if run.receipt.Workflow.State == "needs_review" || !protocol.WorkflowTerminal(run.receipt.Workflow.State) {
			workflowUnknown = true
		}
	}
	a.tasksMu.Unlock()
	if workflowUnknown {
		return errors.New("调度任务停止结果需要确认，未继续重启")
	}
	// Include receipts created by a request already admitted when closing began.
	for _, original := range receipts {
		latest, err := a.taskReceipt(original.Result.TaskID, original.Result.Attempt, owner)
		if err != nil {
			return errors.New("不能核对后台任务停止回执，未继续重启")
		}
		if latest.Result.State == "needs_review" {
			return errors.New("后台任务停止结果需要确认，未继续重启")
		}
	}
	receipts, err = a.immediateTaskReceipts(owner)
	if err != nil {
		return err
	}
	for _, r := range receipts {
		if err := a.stopOwnedReceiptForRestart(ctx, r); err != nil {
			return err
		}
	}
	for _, r := range receipts {
		r.Result.State, r.Result.Detail = "cancelled", "用户确认立即重启 Agent，本次执行及监测已停止；代码和原生历史保留"
		r.Result.DueAt = 0
		if err := a.saveTaskReceipt(r); err != nil {
			return errors.New("不能保存任务停止回执，未继续重启")
		}
		a.publishTask(r)
	}
	// FIFO flush also covers workflow/task final states queued during cancellation.
	// If disconnected, durable receipts remain available to the next Agent.
	_ = a.flushImmediateState()
	return nil
}

// An old receipt does not grant control over the current manual turn. Only a
// turn explicitly recorded as started by that continuation can be interrupted.
func (a *Agent) stopOwnedReceiptForRestart(ctx context.Context, r taskReceipt) error {
	if r.Live == nil || r.Live.OwnTurnID == "" || r.Source.Recovery == nil {
		return nil
	}
	tool := quotaProvider(r.Source.Command, r.Source.Args)
	if tool != "codex" && tool != "opencode" {
		return nil
	}
	if a.ws == nil {
		return errors.New("无法核对后台任务的授权工作区")
	}
	if _, err := a.ws.Resolve(r.Source.Cwd); err != nil {
		return errors.New("后台任务不在授权工作区，未继续重启")
	}
	native, err := a.openLiveControl(ctx, r.Source, BuildEnv(a.cfg, 80, 24), protocol.TaskRunPayload{UserID: r.UserID, Source: r.Source})
	if err != nil {
		return errors.New("后台任务的原生停止状态无法核对")
	}
	defer native.close()
	thread, turn, err := native.readThread(r.Source.Recovery.NativeID)
	if err != nil {
		return errors.New("后台任务回合状态无法核对")
	}
	if thread.ID != r.Source.Recovery.NativeID || !sameDirectory(thread.Cwd, r.Source.Cwd) {
		return errors.New("后台任务身份不匹配，未停止其他回合")
	}
	if turn.ID != r.Live.OwnTurnID || turn.Status != "inProgress" {
		return nil
	}
	var response json.RawMessage
	if err := native.call("turn/interrupt", map[string]any{"threadId": thread.ID, "turnId": turn.ID}, &response); err != nil {
		return errors.New("后台任务停止请求未确认")
	}
	for {
		_, current, err := native.readThread(thread.ID)
		if err != nil {
			return errors.New("后台任务停止结果不明")
		}
		if current.ID != r.Live.OwnTurnID || current.Status == "completed" || current.Status == "failed" || current.Status == "interrupted" {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("后台任务停止等待超时")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (a *Agent) flushImmediateState() error {
	if a.ws == nil {
		return nil
	}
	env, err := a.heartbeatEnvelope()
	if err != nil {
		return err
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		return err
	}
	a.connMu.RLock()
	conn := a.conn
	a.connMu.RUnlock()
	if conn == nil {
		return errors.New("Agent 连接已断开，停止回执已保存在本机")
	}
	return conn.SendTextAndWait(raw)
}

func (a *Agent) immediateTaskReceipts(owner string) ([]taskReceipt, error) {
	entries, err := os.ReadDir(filepath.Join(a.cfg.StateDir, "task-receipts"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("不能核对后台任务回执")
	}
	var result []taskReceipt
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil, errors.New("后台任务回执不可核对")
		}
		data, err := os.ReadFile(filepath.Join(a.cfg.StateDir, "task-receipts", entry.Name()))
		var r taskReceipt
		if err != nil || json.Unmarshal(data, &r) != nil {
			return nil, errors.New("后台任务回执不可核对")
		}
		if r.UserID != owner {
			continue
		}
		switch r.Result.State {
		case "running", "preparing", "waiting_wall", "waiting_quota", "waiting_retry", "waiting_idle", "waiting_agent", "cancelling":
			result = append(result, r)
		}
	}
	return result, nil
}

func (a *Agent) stopNativeForImmediateRestart(ctx context.Context, source protocol.SessionSummary) error {
	tool := quotaProvider(source.Command, source.Args)
	if tool != "codex" && tool != "opencode" {
		return nil
	} // ordinary CLI process trees close with the PTY
	if tool == "codex" {
		for _, arg := range source.Args {
			if arg == "--no-daemon" {
				return nil
			}
		}
	}
	if source.Recovery == nil || source.Recovery.NativeID == "" {
		return fmt.Errorf("%s 会话缺少原生绑定，无法确认后台模型已停止", tool)
	}
	if a.ws == nil {
		return errors.New("缺少授权工作区，未停止原生任务")
	}
	if _, err := a.ws.Resolve(source.Cwd); err != nil {
		return errors.New("原生任务的工作目录不在授权范围，未继续重启")
	}
	if tool == "opencode" {
		return a.closeOpenCodeV2SessionContext(ctx, source)
	}
	for i, arg := range source.Args {
		if strings.HasPrefix(arg, "--remote=") && arg != "--remote=unix://" || arg == "--remote" && i+1 < len(source.Args) && source.Args[i+1] != "unix://" {
			return errors.New("存在自定义 Codex 控制端点，请先在原会话停止任务")
		}
	}
	control, err := openCodexControl(ctx, codexHome(BuildEnv(a.cfg, 80, 24)))
	if err != nil {
		return errors.New("无法核对 Codex 后台任务是否停止")
	}
	defer control.close()
	var read struct {
		Thread nativeThread `json:"thread"`
	}
	if err := control.call("thread/read", map[string]any{"threadId": source.Recovery.NativeID, "includeTurns": false}, &read); err != nil {
		return errors.New("无法核对原 Codex 对话状态")
	}
	if read.Thread.ID != source.Recovery.NativeID || !sameDirectory(read.Thread.Cwd, source.Cwd) {
		return errors.New("Codex 原生对话与工作目录不匹配，未停止其他任务")
	}
	if read.Thread.Status.Type == "notLoaded" || read.Thread.Status.Type == "idle" {
		return nil
	}
	_, turn, err := control.readThread(source.Recovery.NativeID)
	if err != nil {
		return errors.New("无法核对 Codex 当前回合，未继续重启")
	}
	if turn.Status != "inProgress" {
		switch turn.Status {
		case "completed", "failed", "interrupted":
			return nil
		}
		return errors.New("Codex 回合状态不明，未继续重启")
	}
	var response json.RawMessage
	if err := control.call("turn/interrupt", map[string]any{"threadId": source.Recovery.NativeID, "turnId": turn.ID}, &response); err != nil {
		return errors.New("Codex 停止请求未确认，未继续重启")
	}
	for {
		_, current, err := control.readThread(source.Recovery.NativeID)
		if err != nil {
			return errors.New("Codex 停止结果不明，未继续重启")
		}
		if current.Status != "inProgress" {
			switch current.Status {
			case "completed", "failed", "interrupted":
				return nil
			}
			return errors.New("Codex 停止结果不明，未继续重启")
		}
		if current.ID != turn.ID {
			return errors.New("Codex 出现新的回合，请核对原会话后重试")
		}
		select {
		case <-ctx.Done():
			return errors.New("Codex 停止等待超时，未继续重启")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
