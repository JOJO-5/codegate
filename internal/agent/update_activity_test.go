package agent

import (
	"context"
	"errors"
	"github.com/jojo/codegate/internal/protocol"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
)

func idleActivityAgent(t *testing.T) *Agent {
	t.Helper()
	mgr := session.NewManager(func(context.Context, terminal.StartConfig) (terminal.Terminal, error) {
		return nil, errors.New("test factory")
	}, session.Config{})
	t.Cleanup(func() { mgr.CloseAll("test") })
	return &Agent{mgr: mgr}
}
func TestUpdateBarrierIncludesActivitiesAndPreventsNewWork(t *testing.T) {
	a := idleActivityAgent(t)
	done, err := a.beginActivity()
	if err != nil {
		t.Fatal(err)
	}
	if a.FreezeForUpdate() {
		t.Fatal("froze during active Git/worktree/Web operation")
	}
	done()
	if !a.FreezeForUpdate() {
		t.Fatal("idle freeze rejected")
	}
	if _, err := a.beginActivity(); !errors.Is(err, ErrUpdateBusy) {
		t.Fatal("new work crossed barrier")
	}
}
func TestUpdateBarrierRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := idleActivityAgent(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var frozen bool
		var release func()
		var err error
		go func() { defer wg.Done(); <-start; frozen = a.FreezeForUpdate() }()
		go func() { defer wg.Done(); <-start; release, err = a.beginActivity() }()
		close(start)
		wg.Wait()
		if frozen && err == nil {
			t.Fatal("freeze and activity both succeeded")
		}
		if release != nil {
			release()
		}
	}
}
func TestPrivateAgentStateExcludedFromBroadWorkspace(t *testing.T) {
	ws, root := newTestWorkspace(t)
	state := filepath.Join(root, "private-agent")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ws.ExcludePrivateState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ResolveIn(root, "private-agent/updates/current.json"); !errors.Is(err, ErrPathNotAllowed) {
		t.Fatalf("state visible: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(state, alias); err == nil {
		if _, err := ws.Resolve(alias); !errors.Is(err, ErrPathNotAllowed) {
			t.Fatal("private state reachable by alias")
		}
	}
	if _, err := ws.ResolveIn(root, "project/file.txt"); err != nil {
		t.Fatal("normal project blocked")
	}
}

func TestGitCommitOperationBlocksUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX blocking hook fixture")
	}
	ws, root := gitFixture(t)
	os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("commit activity\n"), 0600)
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ready > .hook-entered\nwhile [ ! -f .hook-release ]; do sleep 0.02; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(filepath.Join(root, ".hook-release"), []byte("release"), 0600)
	a := idleActivityAgent(t)
	a.ws = ws
	a.log = slog.Default()
	head := strings.TrimSpace(testGit(t, root, "rev-parse", "HEAD"))
	env, err := protocol.NewRequest("commit", protocol.TypeGitRequest, "", protocol.GitPayload{Path: root, Action: "commit", Message: "activity guard", Confirm: true, ExpectedHead: head})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { a.onGitRequest(env); close(finished) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, ".hook-entered")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Git hook never entered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.FreezeForUpdate() {
		t.Fatal("update froze while real git commit was executing")
	}
	os.WriteFile(filepath.Join(root, ".hook-release"), []byte("release"), 0600)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Git commit did not complete")
	}
	if strings.TrimSpace(testGit(t, root, "log", "-1", "--format=%s")) != "activity guard" {
		t.Fatal("Git commit failed")
	}
	if !a.FreezeForUpdate() {
		t.Fatal("completed Git operation still blocks update")
	}
}

func TestManagedProgressDoesNotDependOnServerHeartbeatInterval(t *testing.T) {
	var progress atomic.Int32
	ticker := newHeartbeatTicker(time.Hour, slog.Default(), func(context.Context) error { t.Error("unexpected server heartbeat"); return nil })
	ticker.progressInterval = 5 * time.Millisecond
	ticker.onProgress = func() { progress.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	ticker.run(ctx)
	if progress.Load() < 2 {
		t.Fatal("long server interval prevented local progress")
	}
}
