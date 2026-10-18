package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestWorkflowRestartKeepsReceiptAndNeverReplays(t *testing.T) {
	a := &Agent{cfg: Config{StateDir: t.TempDir()}, log: testLogger()}
	owner := uuid.NewString()
	row := protocol.Workflow{ID: uuid.NewString(), UserID: owner, Goal: "goal", Cwd: "/project", State: "delegating", Revision: 7, MasterNativeID: "ses_master", Worktree: "/preserved"}
	receipt := workflowReceipt{Owner: owner, Hash: workflowHash(&row), Workflow: row, MasterNativeID: row.MasterNativeID}
	if err := a.saveWorkflowReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	// Marshal omits UserID, as on the real Server/Agent wire. The control identity
	// must be restored before comparing the immutable request hash.
	req, err := protocol.NewRequest(uuid.NewString(), protocol.TypeWorkflowControl, "", protocol.WorkflowControl{Action: "start", ID: row.ID, UserID: owner, Workflow: &row})
	if err != nil {
		t.Fatal(err)
	}
	a.onWorkflowControl(req)
	stored, err := a.readWorkflowReceipt(row.ID, owner)
	if err != nil || stored.Workflow.State != "needs_review" || stored.Workflow.Revision != 8 || stored.Workflow.Worktree != "/preserved" {
		t.Fatal(stored, err)
	}
	a.onWorkflowControl(req)
	again, _ := a.readWorkflowReceipt(row.ID, owner)
	if again.Workflow.Revision != 8 || len(a.workflowRuns) != 0 {
		t.Fatal("restart replayed or changed terminal receipt")
	}
	if _, err := a.readWorkflowReceipt(row.ID, uuid.NewString()); err == nil {
		t.Fatal("cross-owner private receipt exposed")
	}
}

func TestWorkflowArtifactIncludesBinaryChangesAndRejectsChangedCommit(t *testing.T) {
	_, root := gitFixture(t)
	ctx := t.Context()
	head := strings.TrimSpace(testGit(t, root, "rev-parse", "HEAD"))
	file := filepath.Join(root, "image.bin")
	if err := os.WriteFile(file, []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := workflowFiles(ctx, root, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte{0, 1, 3}, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := workflowFiles(ctx, root, head)
	if err != nil || before.Fingerprint == after.Fingerprint {
		t.Fatal("binary content was not fingerprinted", err)
	}
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-qm", "unexpected worker commit")
	if _, err := workflowFiles(ctx, root, head); err == nil {
		t.Fatal("changed baseline accepted")
	}
}

func TestWorkflowRestrictionsDoNotDenyProviderAuthentication(t *testing.T) {
	for _, master := range []bool{true, false} {
		blocked := map[string]bool{}
		for _, rule := range workflowOpenCodePermissions(master) {
			if rule["action"] == "*" || rule["action"] == "provider.auth" {
				t.Fatal("tool constraint blocks native authentication")
			}
			blocked[rule["action"]] = rule["effect"] == "deny"
		}
		if !blocked["shell"] || !blocked["subagent"] || master && !blocked["edit"] {
			t.Fatal("execution policy missing", master, blocked)
		}
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, err := workflowCheck(cancelled, t.TempDir(), "git_diff_check"); err != context.Canceled {
		t.Fatal("cancelled verification started", err)
	}
}

func TestWorkflowWorktreeCannotBeCleanedWhileMasterIsUsingIt(t *testing.T) {
	ws, root := gitFixture(t)
	ctx := t.Context()
	path, _, err := createWorktree(ctx, ws, root)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(testGit(t, path, "rev-parse", "HEAD"))
	a := &Agent{workflowRuns: map[string]*workflowRun{"workflow": {receipt: workflowReceipt{Workflow: protocol.Workflow{ID: "workflow", Worktree: path, State: "running"}}}}}
	req := protocol.GitPayload{Path: root, Action: "worktree_remove", Target: path, ExpectedHead: head, Confirm: true}
	if _, err := managedWorktrees(ctx, ws, req, a.backgroundWorkspaceSessions()); err == nil {
		t.Fatal("active workflow worktree was removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("workflow files lost", err)
	}
}
