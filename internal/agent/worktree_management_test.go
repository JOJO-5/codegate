package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jojo/codegate/internal/protocol"
)

func TestManagedWorktreeRealCleanupRetainsBranchAndPrimary(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	path, _, err := createWorktree(ctx, ws, root)
	if err != nil {
		t.Fatal(err)
	}
	list, err := managedWorktrees(ctx, ws, protocol.GitPayload{Path: path, Action: "worktrees"}, nil)
	if err != nil || len(list.Worktrees) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	item := list.Worktrees[0]
	if item.BlockedReason != "" || item.UniqueCommits != 0 || item.Head == "" {
		t.Fatalf("unexpected clean state: %+v", item)
	}
	req := protocol.GitPayload{Path: root, Action: "worktree_remove", Target: path, ExpectedHead: item.Head, Confirm: true}
	result, err := managedWorktrees(ctx, ws, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "分支保留") {
		t.Fatal(result.Message)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("worktree retained", err)
	}
	testGit(t, root, "show-ref", "--verify", "refs/heads/"+item.Branch)
	data, err := os.ReadFile(filepath.Join(root, "tracked.txt"))
	if err != nil || strings.ReplaceAll(string(data), "\r\n", "\n") != "before\n" {
		t.Fatal("primary changed")
	}
}
func TestManagedWorktreeRefusesLocalDataUniqueCommitsAndOccupiedDirectory(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	path, _, err := createWorktree(ctx, ws, root)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(testGit(t, path, "rev-parse", "HEAD"))
	req := protocol.GitPayload{Path: root, Action: "worktree_remove", Target: path, ExpectedHead: head, Confirm: true}
	reject := func(sessions []protocol.SessionSummary) {
		t.Helper()
		if _, err := managedWorktrees(ctx, ws, req, sessions); err == nil {
			t.Fatal("unsafe removal accepted")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("data lost", err)
		}
	}
	os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("keep"), 0600)
	reject(nil)
	os.Remove(filepath.Join(path, "untracked.txt"))
	os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("modified"), 0600)
	reject(nil)
	testGit(t, path, "checkout", "--", "tracked.txt")
	// Ignored local artifacts also block cleanup.
	common := strings.TrimSpace(testGit(t, root, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	f, err := os.OpenFile(filepath.Join(common, "info", "exclude"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\ncache.bin\n")
	f.Close()
	os.WriteFile(filepath.Join(path, "cache.bin"), []byte("local only"), 0600)
	reject(nil)
	os.Remove(filepath.Join(path, "cache.bin"))
	reject([]protocol.SessionSummary{{Cwd: filepath.Join(path, "subdir"), Status: "detached"}})
	os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("unique commit\n"), 0600)
	testGit(t, path, "add", ".")
	testGit(t, path, "commit", "-qm", "unique")
	req.ExpectedHead = strings.TrimSpace(testGit(t, path, "rev-parse", "HEAD"))
	reject(nil)
	list, err := managedWorktrees(ctx, ws, protocol.GitPayload{Path: root, Action: "worktrees"}, nil)
	if err != nil || list.Worktrees[0].UniqueCommits != 1 {
		t.Fatalf("unique: %+v %v", list, err)
	}
	testGit(t, root, "merge", "--ff-only", list.Worktrees[0].Branch)
	req.ExpectedHead = head
	reject(nil) // Stale ticket still cannot delete after merge.
	req.ExpectedHead = list.Worktrees[0].Head
	if _, err := managedWorktrees(ctx, ws, req, nil); err != nil {
		t.Fatal("merged clean worktree should be removable", err)
	}
}
func TestManagedWorktreeRejectsForeignAndUnconfirmedTargets(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	path, _, err := createWorktree(ctx, ws, root)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(testGit(t, path, "rev-parse", "HEAD"))
	for _, req := range []protocol.GitPayload{
		{Path: root, Action: "worktree_remove", Target: path, ExpectedHead: head},
		{Path: root, Action: "worktree_remove", Target: root, ExpectedHead: head, Confirm: true},
		{Path: root, Action: "worktree_remove", Target: t.TempDir(), ExpectedHead: head, Confirm: true},
		{Path: root, Action: "worktree_remove", Target: path, ExpectedHead: "changed", Confirm: true},
	} {
		if _, err := managedWorktrees(ctx, ws, req, nil); err == nil {
			t.Fatal("accepted", req)
		}
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
