package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jojo/codegate/internal/protocol"
)

func testGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return string(out)
}
func gitFixture(t *testing.T) (*Workspace, string) {
	t.Helper()
	ws, root := newTestWorkspace(t)
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Test")
	testGit(t, root, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-qm", "initial")
	return ws, root
}
func TestGitReviewRealRepository(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("after\n"), 0600)
	os.WriteFile(filepath.Join(root, "new file.txt"), []byte("new content\n"), 0600)
	out, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || len(out.Files) != 2 || out.Branch == "" {
		t.Fatalf("status: %+v %v", out, err)
	}
	diff, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "diff", File: "tracked.txt"})
	if err != nil || !strings.Contains(diff.Diff, "+after") {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	testGit(t, root, "add", "tracked.txt")
	staged, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "diff", File: "tracked.txt", Staged: true})
	if err != nil || !strings.Contains(staged.Diff, "+after") {
		t.Fatalf("staged: %+v %v", staged, err)
	}
	fresh, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "diff", File: "new file.txt"})
	if err != nil || !strings.Contains(fresh.Diff, "new content") {
		t.Fatalf("new file: %+v %v", fresh, err)
	}
	if _, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "diff", File: "../outside"}); err == nil {
		t.Fatal("accepted foreign path")
	}
	if _, err := gitReview(ctx, ws, protocol.GitPayload{Path: t.TempDir(), Action: "status"}); err == nil {
		t.Fatal("accepted unauthorized repository")
	}
}
func TestGitReviewRenameAndBoundedOutput(t *testing.T) {
	ws, root := gitFixture(t)
	testGit(t, root, "mv", "tracked.txt", "renamed.txt")
	out, err := gitReview(context.Background(), ws, protocol.GitPayload{Path: root, Action: "diff", File: "renamed.txt", Staged: true})
	if err != nil || len(out.Files) != 1 || out.Files[0].Original != "tracked.txt" || !strings.Contains(out.Diff, "rename") {
		t.Fatalf("rename: %+v %v", out, err)
	}
	os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("long\n", 10000)), 0600)
	out, err = gitReview(context.Background(), ws, protocol.GitPayload{Path: root, Action: "diff", File: "large.txt"})
	if err != nil || !out.Truncated || len(out.Diff) > 13*1024 {
		t.Fatalf("unbounded output %d %v", len(out.Diff), err)
	}
}
func TestGitReviewDoesNotInvokeExternalDiff(t *testing.T) {
	ws, root := gitFixture(t)
	testGit(t, root, "config", "diff.external", "nonexistent-codegate-test-command")
	os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("after\n"), 0600)
	out, err := gitReview(context.Background(), ws, protocol.GitPayload{Path: root, Action: "diff", File: "tracked.txt"})
	if err != nil || !strings.Contains(out.Diff, "+after") {
		t.Fatalf("external diff: %+v %v", out, err)
	}
}
