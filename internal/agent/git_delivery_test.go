package agent

import (
	"context"
	"github.com/jojo/codegate/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitDeliveryRealUpstreamAndRecentCommits(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := context.Background()
	out, err := gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || out.SyncKnown || len(out.Commits) != 1 || out.Commits[0].Subject != "initial" || out.Commits[0].CommittedAt <= 0 {
		t.Fatalf("initial: %+v %v", out, err)
	}
	remote := t.TempDir()
	testGit(t, remote, "init", "--bare")
	branch := strings.TrimSpace(testGit(t, root, "branch", "--show-current"))
	testGit(t, root, "remote", "add", "origin", remote)
	testGit(t, root, "push", "-u", "origin", branch)
	out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || !out.SyncKnown || out.Ahead != 0 || out.Behind != 0 || out.Upstream != "origin/"+branch || out.RepositoryURL != "" {
		t.Fatalf("upstream: %+v %v", out, err)
	}
	os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("local commit\n"), 0600)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-qm", "local task result")
	out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || !out.SyncKnown || out.Ahead != 1 || out.Behind != 0 || len(out.Commits) != 2 || out.Commits[0].Subject != "local task result" {
		t.Fatalf("ahead: %+v %v", out, err)
	}
	clone := t.TempDir()
	testGit(t, clone, "clone", "--branch", branch, remote, ".")
	os.WriteFile(filepath.Join(clone, "remote.txt"), []byte("remote commit"), 0600)
	testGit(t, clone, "add", ".")
	testGit(t, clone, "commit", "-qm", "remote result")
	testGit(t, clone, "push")
	// Before an explicit fetch, the local comparison remains unchanged.
	out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || out.Behind != 0 {
		t.Fatal("unexpected implicit fetch", out, err)
	}
	testGit(t, root, "fetch", "origin")
	out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || out.Ahead != 1 || out.Behind != 1 {
		t.Fatalf("diverged: %+v %v", out, err)
	}
	testGit(t, root, "remote", "set-url", "origin", "git@github.com:JOJO-5/codegate.git")
	out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
	if err != nil || out.RepositoryURL != "https://github.com/JOJO-5/codegate" {
		t.Fatalf("GitHub: %+v %v", out, err)
	}
}
func TestGitHubLinksRejectCredentialsAndForeignHosts(t *testing.T) {
	for _, value := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo.git"} {
		if got := githubRepositoryURL(value); got != "https://github.com/owner/repo" {
			t.Fatal(value, got)
		}
	}
	for _, value := range []string{"javascript:alert(1)", "https://evil.test/owner/repo", "https://github.com.evil.test/owner/repo", "http://github.com/owner/repo", "https://token@github.com/owner/repo", "ssh://git:secret@github.com/owner/repo", "https://github.com:443/owner/repo", "https://github.com/owner/../repo", "https://github.com/owner/repo?token=secret", "https://github.com/owner%2Frepo/x", "git@evil.test:owner/repo.git", "/local/bare.git"} {
		if got := githubRepositoryURL(value); got != "" {
			t.Fatal("unsafe URL accepted", value, got)
		}
	}
}
