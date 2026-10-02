package agent

import "errors"

var ErrUpdateBusy = errors.New("Agent 有操作正在进行，更新已延后")

// Activities are reserved before work starts. The final freeze uses the same
// lock, so Git, worktree creation and Web startup cannot slip past the barrier.
func (a *Agent) beginActivity() (func(), error) {
	a.activityMu.Lock()
	if a.updateFrozen {
		a.activityMu.Unlock()
		return nil, ErrUpdateBusy
	}
	a.activities++
	a.activityGeneration++
	a.activityMu.Unlock()
	return func() { a.activityMu.Lock(); a.activities--; a.activityMu.Unlock() }, nil
}
func (a *Agent) activityState() (int, uint64) {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	return a.activities, a.activityGeneration
}
func (a *Agent) FreezeForUpdate() bool {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	if a.updateFrozen || a.activities != 0 || a.webActive() {
		return false
	}
	if !a.mgr.FreezeIfEmpty() {
		return false
	}
	a.updateFrozen = true
	return true
}
