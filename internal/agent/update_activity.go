package agent

import (
	"context"
	"errors"
)

var ErrUpdateBusy = errors.New("Agent 有操作正在进行，更新已延后")

// Activities are reserved before work starts. The final freeze uses the same
// lock, so Git, worktree creation and Web startup cannot slip past the barrier.
func (a *Agent) beginActivity() (func(), error) {
	a.activityMu.Lock()
	if a.updateFrozen || a.updateDraining {
		a.activityMu.Unlock()
		return nil, ErrUpdateBusy
	}
	a.activities++
	a.activityGeneration++
	a.activityMu.Unlock()
	return func() { a.activityMu.Lock(); a.activities--; a.activityMu.Unlock() }, nil
}

// Owned operations survive connection loss, but can be cancelled by a confirmed
// device restart. The context is never tied to an individual WebSocket.
func (a *Agent) operationContext() context.Context {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	if a.operationCtx == nil {
		a.operationCtx, a.operationCancel = context.WithCancel(context.Background())
		if a.updateDraining {
			a.operationCancel()
		}
	}
	return a.operationCtx
}
func (a *Agent) activityState() (int, uint64) {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	return a.activities, a.activityGeneration
}
func (a *Agent) FreezeForUpdate() bool {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	if a.updateFrozen || a.updateDraining || a.activities != 0 || a.webActive() {
		return false
	}
	if !a.mgr.FreezeIfEmpty() {
		return false
	}
	a.updateFrozen = true
	return true
}

func (a *Agent) drainingForUpdate() bool {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	return a.updateDraining
}
