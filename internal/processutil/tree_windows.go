//go:build windows

package processutil

import (
	"errors"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Tree owns a private job. Children cannot outlive its explicit cleanup.
type Tree struct {
	mu  sync.Mutex
	job windows.Handle
}

// AdoptSuspended owns a newly created process before its primary thread runs.
// The caller owns process/thread handles and must terminate on adoption failure.
func AdoptSuspended(process, thread windows.Handle) (*Tree, error) {
	tree, err := newWindowsTree()
	if err != nil {
		return nil, err
	}
	if err = windows.AssignProcessToJobObject(tree.job, process); err == nil {
		_, err = windows.ResumeThread(thread)
	}
	if err != nil {
		_ = windows.CloseHandle(tree.job)
		return nil, err
	}
	return tree, nil
}

func newWindowsTree() (*Tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &Tree{job: job}, nil
}

// StartTree assigns a suspended process before allowing it to spawn children.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	tree, err := newWindowsTree()
	if err != nil {
		return nil, err
	}
	job := tree.job
	Background(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err = cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	fail := func(err error) (*Tree, error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return fail(err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fail(openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if resumeErr != nil {
			return fail(resumeErr)
		}
		return tree, nil
	}
	return fail(errors.New("suspended process primary thread not found"))
}

func (t *Tree) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return err
	}
	// Termination is asynchronous; wait for descendants to release their sockets.
	type accounting struct {
		UserTime, KernelTime, PeriodUserTime, PeriodKernelTime               int64
		PageFaultCount, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var info accounting
		err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		if err != nil {
			return err
		}
		if info.ActiveProcesses == 0 {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("process tree did not stop in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	err := windows.CloseHandle(t.job)
	if err == nil {
		t.job = 0
	}
	return err
}
