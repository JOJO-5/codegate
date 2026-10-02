package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jojo/codegate/internal/protocol"
)

func TestGitCommitAndPushToRealBareRemote(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	os.MkdirAll(remote, 0700)
	testGit(t, remote, "init", "--bare", "-q")
	testGit(t, root, "remote", "add", "origin", remote)
	branch := strings.TrimSpace(testGit(t, root, "branch", "--show-current"))
	testGit(t, root, "push", "-qu", "origin", branch)
	testGit(t, root, "config", "push.default", "matching")
	testGit(t, root, "branch", "unrelated-task")
	head := strings.TrimSpace(testGit(t, root, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(root, "new file.txt"), []byte("verified mutation\n"), 0600)
	req := protocol.GitPayload{Path: root, Action: "commit", Message: "real commit via Agent", ExpectedHead: head}
	if _, err := gitMutation(ctx, ws, req); err == nil {
		t.Fatal("mutation without confirmation")
	}
	req.Confirm = true
	req.ExpectedHead = "stale"
	if _, err := gitMutation(ctx, ws, req); err == nil {
		t.Fatal("stale HEAD accepted")
	}
	req.ExpectedHead = head
	out, err := gitMutation(ctx, ws, req)
	if err != nil || out.Head == head || len(out.Files) != 0 || out.Message == "" {
		t.Fatalf("commit: %+v %v", out, err)
	}
	if got := testGit(t, root, "show", "HEAD:new file.txt"); got != "verified mutation\n" {
		t.Fatal(got)
	}
	req.Action = "push"
	req.ExpectedHead = out.Head
	out, err = gitMutation(ctx, ws, req)
	if err != nil || out.Message == "" {
		t.Fatalf("push: %+v %v", out, err)
	}
	if got := strings.TrimSpace(testGit(t, remote, "rev-parse", "refs/heads/"+branch)); got != out.Head {
		t.Fatal("remote mismatch")
	}
	if got := strings.TrimSpace(testGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads/")); got != "refs/heads/"+branch {
		t.Fatalf("pushed unrelated refs: %s", got)
	}
	testGit(t, root, "checkout", "--detach", "-q")
	if _, err := gitMutation(ctx, ws, req); err == nil {
		t.Fatal("detached HEAD accepted")
	}
}
func TestGitMutationFailsWithoutChangesOrUpstream(t *testing.T) {
	ws, root := gitFixture(t)
	head := strings.TrimSpace(testGit(t, root, "rev-parse", "HEAD"))
	for _, action := range []string{"commit", "push"} {
		_, err := gitMutation(context.Background(), ws, protocol.GitPayload{Path: root, Action: action, Confirm: true, ExpectedHead: head, Message: "empty"})
		if err == nil {
			t.Fatalf("%s falsely succeeded", action)
		}
	}
}
