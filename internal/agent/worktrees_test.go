package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeIsolationAndRollback(t *testing.T) {
	ws, root := gitFixture(t)
	os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original uncommitted\n"), 0600)
	first, cleanup, err := createWorktree(context.Background(), ws, root)
	if err != nil {
		t.Fatal(err)
	}
	second, cleanup2, err := createWorktree(context.Background(), ws, root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(first, ".codegate-worktrees") {
		t.Fatalf("not isolated: %s %s", first, second)
	}
	data, err := os.ReadFile(filepath.Join(first, "tracked.txt"))
	if err != nil || string(data) != "before\n" {
		t.Fatalf("copied dirty source: %q %v", data, err)
	}
	os.WriteFile(filepath.Join(first, "tracked.txt"), []byte("task one\n"), 0600)
	data, _ = os.ReadFile(filepath.Join(second, "tracked.txt"))
	if string(data) != "before\n" {
		t.Fatal("tasks share files")
	}
	data, _ = os.ReadFile(filepath.Join(root, "tracked.txt"))
	if string(data) != "original uncommitted\n" {
		t.Fatal("source changed")
	}
	status := testGit(t, root, "status", "--porcelain")
	if strings.Contains(status, ".codegate-worktrees") {
		t.Fatal("managed tasks pollute source status")
	}
	cleanup2()
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("clean failed task remains")
	}
	cleanup()
	if _, err := os.Stat(first); err != nil {
		t.Fatal("dirty worktree deleted")
	}
	branch := strings.TrimSpace(testGit(t, first, "branch", "--show-current"))
	if !strings.HasPrefix(branch, "codegate/task-") {
		t.Fatal(branch)
	}
	if _, err := gitRoot(context.Background(), ws, first); err != nil {
		t.Fatalf("cannot review created worktree: %v", err)
	}
}
func TestWorktreeRequiresCommitAndRejectsEscapingParent(t *testing.T) {
	ws, root := newTestWorkspace(t)
	testGit(t, root, "init", "-q")
	if _, _, err := createWorktree(context.Background(), ws, root); err == nil {
		t.Fatal("accepted unborn repository")
	}
	ws, root = gitFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".codegate-worktrees")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := createWorktree(context.Background(), ws, root); err == nil {
		t.Fatal("accepted escaping worktree parent")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("created files outside workspace")
	}
}
