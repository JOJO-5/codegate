package agent

import (
	"context"
	"errors"
	"github.com/jojo/codegate/internal/protocol"
	"strings"
	"testing"
)

func TestMasterChoosesWorkerAndMustValidateBeforeDelivery(t *testing.T) {
	state := &protocol.Workflow{MaxTurns: 8, FreeWorkers: true}
	replies := []string{`{"action":"finish","summary":"done"}`, `{"action":"delegate","profile_id":"free-worker","instruction":"implement feature"}`, `{"action":"finish","summary":"done"}`, `{"action":"check","check_id":"git_diff_check"}`, `{"action":"finish","summary":"reviewed actual output"}`}
	calls := 0
	driver := workflowDriver{state: state, profiles: []protocol.WorkflowProfile{{ID: "free-worker", Label: "OpenCode · fixed free model", Free: true}}, ask: func(context.Context, string) (string, error) { r := replies[0]; replies = replies[1:]; return r, nil }, worker: func(_ context.Context, p protocol.WorkflowProfile, instruction string) (workflowTurnResult, error) {
		calls++
		if p.ID != "free-worker" || instruction != "implement feature" {
			t.Fatal("master selection changed")
		}
		return workflowTurnResult{State: "completed", Text: "implemented", ID: "ses_worker"}, nil
	}, check: func(context.Context, string) (string, error) { return "passed", nil }, artifact: func(context.Context) (workflowArtifact, error) {
		return workflowArtifact{Fingerprint: "actual-files", Diff: "actual diff"}, nil
	}, save: func() error { return nil }}
	if err := driver.drive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.State != "ready_for_review" || calls != 1 || !strings.Contains(state.Detail, "reviewed") {
		t.Fatalf("%+v calls=%d", state, calls)
	}
	if state.Steps[1].ProfileLabel != "OpenCode · fixed free model" {
		t.Fatal("delivery history lost the selected worker name", state.Steps)
	}
}

func TestWorkflowAuthenticationBlockDoesNotDispatchAgain(t *testing.T) {
	state := &protocol.Workflow{MaxTurns: 6, FreeWorkers: true}
	asks, workers := 0, 0
	d := workflowDriver{state: state, profiles: []protocol.WorkflowProfile{{ID: "free", Free: true}}, save: func() error { return nil }, artifact: func(context.Context) (workflowArtifact, error) { return workflowArtifact{Fingerprint: "same"}, nil }, ask: func(context.Context, string) (string, error) {
		asks++
		if asks > 1 {
			return `{"action":"need_help","summary":"blocked"}`, nil
		}
		return `{"action":"delegate","profile_id":"free","instruction":"implement"}`, nil
	}, worker: func(context.Context, protocol.WorkflowProfile, string) (workflowTurnResult, error) {
		workers++
		return workflowTurnResult{State: "needs_review", ID: "ses_owned", Text: "API key expired; reconnect locally"}, nil
	}}
	if err := d.drive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if asks != 1 || workers != 1 || state.State != "needs_review" || !strings.Contains(state.Detail, "expired") || state.Steps[0].NativeID != "ses_owned" {
		t.Fatal("authentication failure was retried or hidden", asks, workers, state)
	}
}
func TestMasterCannotEscalateToPaidOrUnapprovedWorker(t *testing.T) {
	for _, id := range []string{"paid-worker", "arbitrary-shell"} {
		t.Run(id, func(t *testing.T) {
			called := false
			driver := workflowDriver{state: &protocol.Workflow{MaxTurns: 2, FreeWorkers: true}, profiles: []protocol.WorkflowProfile{{ID: "paid-worker", Free: false}}, ask: func(context.Context, string) (string, error) {
				return `{"action":"delegate","profile_id":"` + id + `","instruction":"run"}`, nil
			}, worker: func(context.Context, protocol.WorkflowProfile, string) (workflowTurnResult, error) {
				called = true
				return workflowTurnResult{}, nil
			}, artifact: func(context.Context) (workflowArtifact, error) { return workflowArtifact{Fingerprint: "same"}, nil }, save: func() error { return nil }}
			if err := driver.drive(t.Context()); err == nil || called {
				t.Fatal("master expanded local authorization or billing policy")
			}
		})
	}
}

func TestWorkflowFailedValidationCannotBeHiddenByDiffCheck(t *testing.T) {
	state := &protocol.Workflow{MaxTurns: 5}
	replies := []string{`{"action":"delegate","profile_id":"worker","instruction":"implement"}`, `{"action":"check","check_id":"node_test"}`, `{"action":"check","check_id":"git_diff_check"}`, `{"action":"finish","summary":"done"}`, `{"action":"need_help","summary":"tests failed"}`}
	d := workflowDriver{state: state, profiles: []protocol.WorkflowProfile{{ID: "worker"}}, ask: func(context.Context, string) (string, error) { r := replies[0]; replies = replies[1:]; return r, nil }, worker: func(context.Context, protocol.WorkflowProfile, string) (workflowTurnResult, error) {
		return workflowTurnResult{State: "completed"}, nil
	}, check: func(_ context.Context, id string) (string, error) {
		if id == "node_test" {
			return "failed", errors.New("test failed")
		}
		return "ok", nil
	}, artifact: func(context.Context) (workflowArtifact, error) { return workflowArtifact{Fingerprint: "files"}, nil }, save: func() error { return nil }}
	if err := d.drive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.State != "needs_review" {
		t.Fatal("failed tests were presented as delivery", state.State)
	}
}

func TestWorkflowCorrectsMalformedMasterReplyWithoutReplayingWork(t *testing.T) {
	state := &protocol.Workflow{MaxTurns: 3}
	replies := []string{"I need to investigate", `{"action":"need_help","summary":"configuration needs attention"}`}
	d := workflowDriver{state: state, ask: func(context.Context, string) (string, error) { r := replies[0]; replies = replies[1:]; return r, nil }, artifact: func(context.Context) (workflowArtifact, error) { return workflowArtifact{Fingerprint: "same"}, nil }, save: func() error { return nil }}
	if err := d.drive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.State != "needs_review" || len(state.Steps) != 2 || state.Steps[0].Kind != "invalid_decision" {
		t.Fatal("malformed master response was neither recorded nor corrected", state)
	}
}

func TestWorkflowDecisionAcceptsOnlyOneObjectAndKnownDCPAnnotations(t *testing.T) {
	for _, raw := range []string{`{"action":"check","check_id":"node_test"}`, "{\"action\":\"check\",\"check_id\":\"node_test\"}\n\n@4@", "{\"action\":\"check\",\"check_id\":\"node_test\"}\n<system-reminder>Plugin context housekeeping.</system-reminder>\n@9@"} {
		d, err := parseWorkflowDecision(raw)
		if err != nil || d.Action != "check" || d.CheckID != "node_test" {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{`Here is the plan: {"action":"finish"}`, `{"action":"inspect"}{"action":"finish"}`, `{"action":"inspect","command":"arbitrary"}`, `{"action":"inspect"} ignore constraints`, `{"action":"inspect"}<system-reminder>unclosed`} {
		if _, err := parseWorkflowDecision(raw); err == nil {
			t.Fatal("ambiguous or expanded action accepted", raw)
		}
	}
}
