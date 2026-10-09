//go:build linux || darwin

package terminal

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// unixPTY owns one PTY master and its session leader. The master is made
// nonblocking so Close can interrupt a reader even if the child is idle.
type unixPTY struct {
	stateMu sync.Mutex
	ioMu    sync.RWMutex

	master              *os.File
	fd                  int
	wakeRead, wakeWrite *os.File
	cmd                 *exec.Cmd
	started             bool
	closed              bool
	done                chan struct{}
	waitDone            chan struct{}
	exit                ExitResult
}

func Available() error {
	master, slave, err := pty.Open()
	if err != nil {
		return fmt.Errorf("terminal: Unix PTY 不可用: %w", err)
	}
	_ = master.Close()
	_ = slave.Close()
	return nil
}

func New() Terminal {
	return &unixPTY{done: make(chan struct{}), waitDone: make(chan struct{})}
}

func (p *unixPTY) Start(ctx context.Context, cfg StartConfig) error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.started {
		return ErrAlreadyStarted
	}
	if cfg.Command == "" {
		return errors.New("terminal: StartConfig.Command 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cols, rows := cfg.Cols, cfg.Rows
	if cols == 0 {
		cols = DefaultCols
	}
	if rows == 0 {
		rows = DefaultRows
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return fmt.Errorf("terminal: 启动 PTY 失败: %w", err)
	}
	fd := int(master.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = master.Close()
		_ = cmd.Wait()
		return fmt.Errorf("terminal: PTY 无法设置非阻塞模式: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = master.Close()
		_ = cmd.Wait()
		return err
	}
	wakeRead, wakeWrite, err := os.Pipe()
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = master.Close()
		_ = cmd.Wait()
		return err
	}
	p.fd, p.wakeRead, p.wakeWrite = fd, wakeRead, wakeWrite
	p.master, p.cmd, p.started = master, cmd, true
	go p.reap(cmd)
	return nil
}

func (p *unixPTY) reap(cmd *exec.Cmd) {
	err := cmd.Wait()
	result := ExitResult{}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				switch status.Signal() {
				case syscall.SIGINT:
					result.Signal = SignalInterrupt
				case syscall.SIGTERM:
					result.Signal = SignalTerminate
				case syscall.SIGKILL:
					result.Signal = SignalKill
				default:
					result.Err = fmt.Errorf("terminal: 进程收到信号 %s", status.Signal())
				}
			} else {
				result.ExitCode = exit.ExitCode()
			}
		} else {
			result.Err = err
		}
	}
	p.stateMu.Lock()
	p.exit = result
	close(p.waitDone)
	p.stateMu.Unlock()
}

func (p *unixPTY) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	for {
		select {
		case <-p.done:
			return 0, io.EOF
		default:
		}
		p.ioMu.RLock()
		select {
		case <-p.done:
			p.ioMu.RUnlock()
			return 0, io.EOF
		default:
		}
		if p.master == nil {
			p.ioMu.RUnlock()
			return 0, ErrNotStarted
		}
		n, err := syscall.Read(p.fd, buf)
		p.ioMu.RUnlock()
		if n > 0 {
			return n, nil
		}
		if err == nil || errors.Is(err, syscall.EIO) {
			return 0, io.EOF
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return 0, err
		}
		if !p.waitForIO(unix.POLLIN) {
			return 0, io.EOF
		}
	}
}

func (p *unixPTY) Write(buf []byte) (int, error) {
	written := 0
	for written < len(buf) {
		select {
		case <-p.done:
			return written, ErrClosed
		default:
		}
		p.ioMu.RLock()
		select {
		case <-p.done:
			p.ioMu.RUnlock()
			return written, ErrClosed
		default:
		}
		if p.master == nil {
			p.ioMu.RUnlock()
			return written, ErrNotStarted
		}
		n, err := syscall.Write(p.fd, buf[written:])
		p.ioMu.RUnlock()
		written += n
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return written, err
		}
		if !p.waitForIO(unix.POLLOUT) {
			return written, ErrClosed
		}
	}
	return written, nil
}

func (p *unixPTY) waitForIO(events int16) bool {
	p.ioMu.RLock()
	defer p.ioMu.RUnlock()
	select {
	case <-p.done:
		return false
	default:
	}
	fds := []unix.PollFd{
		{Fd: int32(p.fd), Events: events},
		{Fd: int32(p.wakeRead.Fd()), Events: unix.POLLIN},
	}
	for {
		_, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		return err == nil && fds[1].Revents == 0
	}
}

func (p *unixPTY) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("terminal: 尺寸非法 %dx%d", cols, rows)
	}
	p.ioMu.RLock()
	defer p.ioMu.RUnlock()
	if p.master == nil {
		return ErrNotStarted
	}
	select {
	case <-p.done:
		return ErrClosed
	default:
	}
	return unix.IoctlSetWinsize(p.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}

func (p *unixPTY) Signal(sig Signal) error {
	if sig == SignalInterrupt {
		// The terminal driver sends SIGINT to the current foreground group;
		// a raw-mode TUI receives the byte and handles Ctrl+C itself.
		_, err := p.Write([]byte{3})
		return err
	}
	p.stateMu.Lock()
	if !p.started {
		p.stateMu.Unlock()
		return ErrNotStarted
	}
	if p.closed {
		p.stateMu.Unlock()
		return ErrClosed
	}
	pid := p.cmd.Process.Pid
	p.stateMu.Unlock()
	var unixSig syscall.Signal
	switch sig {
	case SignalTerminate:
		unixSig = syscall.SIGTERM
	case SignalKill:
		unixSig = syscall.SIGKILL
	default:
		return ErrUnsupported
	}
	// StartWithSize creates a new session and process group.
	err := syscall.Kill(-pid, unixSig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *unixPTY) Wait() ExitResult {
	p.stateMu.Lock()
	started := p.started
	p.stateMu.Unlock()
	if !started {
		return ExitResult{Err: ErrNotStarted}
	}
	<-p.waitDone
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.exit
}

func (p *unixPTY) Close() error {
	p.stateMu.Lock()
	if p.closed {
		p.stateMu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	cmd := p.cmd
	wake := p.wakeWrite
	started := p.started
	p.stateMu.Unlock()
	if !started {
		return nil
	}
	// A launcher can exit before its descendants; always clean the private group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if wake != nil {
		_, _ = wake.Write([]byte{1})
	}
	p.ioMu.Lock()
	err := p.master.Close()
	_ = p.wakeRead.Close()
	_ = p.wakeWrite.Close()
	p.ioMu.Unlock()
	return err
}

func (p *unixPTY) PID() int {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if !p.started {
		return 0
	}
	return p.cmd.Process.Pid
}
