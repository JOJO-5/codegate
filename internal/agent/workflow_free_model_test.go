package agent

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

// Explicit opt-in only. All hosted calls use a currently confirmed zero-price
// profile. No paid Codex calls or changes to the user's working repository.
func TestWorkflowFreeModelCodesAndValidatesInWorktree(t *testing.T) {
	executable := os.Getenv("CODEGATE_OPENCODE_V2_SMOKE")
	if os.Getenv("CODEGATE_WORKFLOW_FREE_SMOKE") != "1" || executable == "" {
		t.Skip("real orchestration smoke not requested")
	}
	_, root := gitFixture(t)
	cfg := DefaultConfig()
	cfg.ServerURL = "https://localhost.invalid"
	cfg.StateDir = t.TempDir()
	cfg.AllowedRoots = []string{root}
	cfg.AllowedCommands = []CommandSpec{{ID: "opencode", Label: "OpenCode", Command: executable, Kind: KindTUI}}
	a, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	a.quotaCtx = ctx
	profiles, err := a.workflowProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var master protocol.WorkflowProfile
	selection := os.Getenv("CODEGATE_OPENCODE_V2_FREE_MODEL")
	if selection == "" {
		selection = "opencode/mimo-v2.6-flash-free"
	}
	for _, p := range profiles {
		if p.Provider+"/"+p.Model == selection && p.Free {
			master = p
			break
		}
	}
	if master.ID == "" {
		t.Fatal("confirmed free tool-capable model unavailable; no hosted call made")
	}
	id, owner := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		cancel()
		a.stopBackgroundTasks()
		receipt, e := a.readWorkflowReceipt(id, owner)
		if e != nil {
			return
		}
		cwd := receipt.Workflow.Worktree
		if cwd == "" {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		client, e := connectOpenCodeV2(cleanup, ResolvedCommand{Command: executable}, BuildEnv(cfg, 80, 24), cwd)
		if e != nil {
			t.Error(e)
			return
		}
		ids := map[string]bool{receipt.MasterNativeID: true}
		for _, step := range receipt.Workflow.Steps {
			if step.NativeID != "" {
				ids[step.NativeID] = true
			}
		}
		for native := range ids {
			if native == "" {
				continue
			}
			var session struct {
				Data struct {
					Cost *float64 `json:"cost"`
				} `json:"data"`
			}
			if e := client.call(cleanup, "GET", "/api/session/"+native, nil, &session); e != nil {
				t.Error(e)
			} else if session.Data.Cost == nil || *session.Data.Cost != 0 {
				t.Error("nonzero native session cost", session.Data.Cost)
			}
			_ = client.call(cleanup, "POST", "/api/session/"+native+"/interrupt?resume=false", nil, nil)
			if e := client.call(cleanup, "DELETE", "/api/session/"+native, nil, nil); e != nil {
				t.Error(e)
			}
		}
		if e := client.call(cleanup, "DELETE", "/api/debug/location?"+url.Values{"location[directory]": {cwd}}.Encode(), nil, nil); e != nil {
			t.Error(e)
		}
	})
	goal := "Implement sum.mjs exporting add(a,b) and sum.test.mjs with node:test and node:assert/strict proving add(2,3)===5. Do not change tracked.txt, commit, push, or deploy. For this short smoke use worker profile " + master.ID + ". Choose delegate, then check node_test, then check git_diff_check, then finish. Workers should use native file editing tools. Keep files small."
	row := &protocol.Workflow{ID: id, UserID: owner, DeviceID: a.deviceUUID().String(), Goal: goal, Cwd: root, MasterID: master.ID, FreeWorkers: true, MaxTurns: 10, MaxMinutes: 3, Revision: 1}
	req, err := protocol.NewRequest(uuid.NewString(), protocol.TypeWorkflowControl, "", protocol.WorkflowControl{Action: "start", ID: id, UserID: owner, Workflow: row})
	if err != nil {
		t.Fatal(err)
	}
	a.onWorkflowControl(req)
	var receipt workflowReceipt
	for {
		receipt, err = a.readWorkflowReceipt(id, owner)
		if err == nil && protocol.WorkflowTerminal(receipt.Workflow.State) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("workflow timed out", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	a.tasksWG.Wait()
	if receipt.Workflow.State != "ready_for_review" {
		t.Fatalf("orchestration not delivered: %s %s steps=%+v", receipt.Workflow.State, receipt.Workflow.Detail, receipt.Workflow.Steps)
	}
	if receipt.MasterNativeID == "" || !strings.Contains(receipt.Workflow.Diff, "sum.mjs") {
		t.Fatal("missing actual evidence", receipt.Workflow)
	}
	if _, err := os.Stat(filepath.Join(root, "sum.mjs")); !os.IsNotExist(err) {
		t.Fatal("original repository modified")
	}
	if out, err := workflowCheck(ctx, receipt.Workflow.Worktree, "node_test"); err != nil {
		t.Fatal(out, err)
	}
	t.Logf("real free master/worker delivered %d decisions; actual node tests passed; original repository unchanged", len(receipt.Workflow.Steps))
}
