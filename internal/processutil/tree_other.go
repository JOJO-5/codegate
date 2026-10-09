//go:build !windows

package processutil

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type Tree struct {
	mu  sync.Mutex
	pid int
}

func StartTree(cmd *exec.Cmd) (*Tree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Tree{pid: cmd.Process.Pid}, nil
}

func (t *Tree) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pid == 0 {
		return nil
	}
	err := syscall.Kill(-t.pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	t.pid = 0
	return nil
}
