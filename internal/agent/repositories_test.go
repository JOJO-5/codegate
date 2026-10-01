package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanRepositoriesNestedWorktreesAndOverlappingRoots(t *testing.T) {
	_, root := newTestWorkspace(t)
	paths := []string{root, filepath.Join(root, "team", "service"), filepath.Join(root, "team", "service", "submodule"), filepath.Join(root, ".hidden", "repo"), filepath.Join(root, "node_modules", "nested-repo")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere/repo/.git/worktrees/worktree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, worktree)
	// Git metadata must not appear as a discovered working tree.
	if err := os.MkdirAll(filepath.Join(root, ".git", "metadata", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	result := scanRepositories(context.Background(), NewWorkspace([]string{root, filepath.Join(root, "team")}))
	if result.Partial || result.Total != len(paths) {
		t.Fatalf("unexpected scan: %+v", result)
	}
	got := map[string]bool{}
	for _, repo := range result.Repositories {
		got[repo.Path] = true
	}
	for _, path := range paths {
		if !got[path] {
			t.Errorf("missing %s", path)
		}
	}
}

func TestScanRepositoriesDoesNotEscapeWorkspace(t *testing.T) {
	_, root := newTestWorkspace(t)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	result := scanRepositories(context.Background(), NewWorkspace([]string{root}))
	if result.Total != 0 {
		t.Fatalf("escaped workspace: %+v", result)
	}
}

func TestScanRepositoriesReportsMissingRootsAndCancellation(t *testing.T) {
	_, root := newTestWorkspace(t)
	result := scanRepositories(context.Background(), NewWorkspace([]string{filepath.Join(root, "missing")}))
	if !result.Partial || result.ErrorCount != 1 || len(result.Warnings) != 1 {
		t.Fatalf("missing root silently ignored: %+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = scanRepositories(ctx, NewWorkspace([]string{root}))
	if !result.Partial {
		t.Fatal("cancelled scan claimed to be complete")
	}
}
