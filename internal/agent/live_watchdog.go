package agent

import (
	"context"
	"github.com/jojo/codegate/internal/protocol"
	"time"
)

type liveNativeRun struct {
	turnID string
	cancel context.CancelFunc
}

// The local guard remains effective if Server/browser disconnect. Only the
// native turn started by this task can be interrupted; manual/new turns win.
func (a *Agent) trackLiveRun(p protocol.TaskRunPayload, receipt taskReceipt) {
	finish, e := a.beginActivity()
	if e != nil {
		return
	}
	a.quotaMu.Lock()
	parent := a.quotaCtx
	a.quotaMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	key := taskRunKey(p.TaskID, p.Attempt)
	run := &liveNativeRun{turnID: receipt.Live.OwnTurnID, cancel: cancel}
	a.tasksMu.Lock()
	if a.tasksClosing {
		a.tasksMu.Unlock()
		cancel()
		finish()
		return
	}
	if old := a.liveRuns[key]; old != nil && old.turnID == run.turnID {
		a.tasksMu.Unlock()
		cancel()
		finish()
		return
	}
	if a.liveRuns == nil {
		a.liveRuns = map[string]*liveNativeRun{}
	}
	if old := a.liveRuns[key]; old != nil {
		old.cancel()
	}
	a.liveRuns[key] = run
	a.tasksWG.Add(1)
	a.tasksMu.Unlock()
	go func() {
		defer a.tasksWG.Done()
		defer finish()
		defer cancel()
		defer func() {
			a.tasksMu.Lock()
			if a.liveRuns[key] == run {
				delete(a.liveRuns, key)
			}
			a.tasksMu.Unlock()
		}()
		timer := time.NewTicker(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				a.inspectOwnedLiveRun(p, run.turnID, true)
				return
			case <-timer.C:
				if a.inspectOwnedLiveRun(p, run.turnID, false) {
					return
				}
			}
		}
	}()
}
func (a *Agent) inspectOwnedLiveRun(p protocol.TaskRunPayload, owned string, stopping bool) bool {
	a.liveQuotaMu.Lock()
	defer a.liveQuotaMu.Unlock()
	r, e := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	if e != nil || r.Live == nil || r.Live.OwnTurnID != owned || r.Result.State != "running" {
		return true
	}
	sess, ok := a.mgr.Get(parseUUID(p.Source.SessionID))
	if !ok {
		return true
	}
	// Any person typing takes responsibility for the native turn. Do not cancel it.
	if sess.InputRevision() != r.Live.InputRevision {
		r.Result.State = "cancelled"
		r.Result.Detail = "检测到手动输入，已让出自动续跑控制"
		a.publishTask(r)
		return true
	}
	ended := !isRecoverableLive(sess.Summary())
	expired := time.Now().UnixMilli() >= r.Live.StartedAt+int64(p.MaxRunMinutes)*60000
	ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	native, e := a.openLiveControl(ctx, sess.Summary(), BuildEnv(a.cfg, 80, 24), p)
	if e != nil {
		if stopping || ended || expired {
			r.Result.State = "needs_review"
			r.Result.Detail = "无法连接原生控制接口确认本次回合停止，请核对原对话"
			a.publishTask(r)
			return true
		}
		return false
	}
	defer native.close()
	_, turn, e := native.readThread(p.Source.Recovery.NativeID)
	if e != nil {
		if stopping || ended || expired {
			r.Result.State = "needs_review"
			r.Result.Detail = "无法连接原生控制接口确认本次回合停止，请核对原对话"
			a.publishTask(r)
			return true
		}
		return false
	}
	if turn.ID != owned {
		r.Result.State = "cancelled"
		r.Result.Detail = "原对话已产生新回合，已关闭自动控制"
		a.publishTask(r)
		return true
	}
	if turn.Status != "inProgress" {
		return true
	} // Server's next poll records the final error/success and owns further retries.
	if !stopping && !ended && !expired {
		return false
	}
	var out any
	e = native.call("turn/interrupt", map[string]any{"threadId": p.Source.Recovery.NativeID, "turnId": owned}, &out)
	r.Result.State = "cancelled"
	r.Result.Detail = "Agent或原终端退出，已停止本次自动回合"
	if expired {
		r.Result.State = "failed"
		r.Result.Detail = "达到本次运行上限，已停止自动回合并保留原终端"
	}
	if e != nil {
		r.Result.State = "needs_review"
		r.Result.Detail = "无法确认本次自动回合停止，请核对原生对话"
	}
	a.publishTask(r)
	return true
}
