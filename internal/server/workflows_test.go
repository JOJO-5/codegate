package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestWorkflowHTTPAuthorizationPersistenceNonceAndOfflineCancellation(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "workflow-owner@example.com", "correct-horse-battery")
	other := e.signup(t, "workflow-other@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, _ := e.seedPairedDevice(t, owner)
	path := "/api/v1/devices/" + device + "/workflows"
	body := map[string]any{"nonce": uuid.NewString(), "goal": "implement goal", "cwd": "/project", "master_id": "local-profile"}
	if r := e.post(t, path, other, body); r.Status != 404 {
		t.Fatal("cross-account create", r.Status)
	}
	body["command"] = "unapproved-executable"
	if r := e.post(t, path, token, body); r.Status != 400 {
		t.Fatal("forged command accepted", r.Status)
	}
	delete(body, "command")
	r := e.post(t, path, token, body)
	if r.Status != 201 {
		t.Fatalf("create %d %s", r.Status, r.Body)
	}
	var row protocol.Workflow
	if err := json.Unmarshal(r.Body, &row); err != nil {
		t.Fatal(err)
	}
	if row.State != "queued" || !row.FreeWorkers || row.MaxTurns != 12 {
		t.Fatal(row)
	}
	r = e.post(t, path, token, body)
	if r.Status != 200 {
		t.Fatal("nonce retry", r.Status)
	}
	var repeated protocol.Workflow
	_ = json.Unmarshal(r.Body, &repeated)
	if repeated.ID != row.ID {
		t.Fatal("duplicate execution")
	}
	body["goal"] = "different goal"
	if r = e.post(t, path, token, body); r.Status != 409 {
		t.Fatal("nonce reused for different goal", r.Status)
	}
	body["goal"] = "implement goal"
	e.srv.pollWorkflows(t.Context())
	persisted, err := e.store.Workflow(t.Context(), row.ID)
	if err != nil || persisted.State != "waiting_agent" {
		t.Fatal(persisted, err)
	}
	if r = e.post(t, path+"/"+row.ID+"/cancel", other, nil); r.Status != 404 {
		t.Fatal("cross-account cancel")
	}
	if r = e.post(t, path+"/"+row.ID+"/cancel", token, nil); r.Status != 200 {
		t.Fatal(r.Status)
	}
	e.srv.pollWorkflows(t.Context())
	persisted, _ = e.store.Workflow(t.Context(), row.ID)
	if persisted.State != "cancelled" {
		t.Fatal("cancelled queue was launched")
	}
}

func TestWorkflowDispatchDoesNotReplayAndCancellationDoesNotRegress(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "workflow-dispatch@example.com", "correct-horse-battery")
	owner := e.meUserID(t, token)
	device, private := e.seedPairedDevice(t, owner)
	agent := e.agentHandshake(t, device, private, protocol.AgentCaps{WorkflowV1: true})
	row := &protocol.Workflow{ID: uuid.NewString(), UserID: owner, DeviceID: device, Goal: "goal", Cwd: "/project", MasterID: "profile", State: "queued", Revision: 1, MaxMinutes: 1, MaxTurns: 4}
	if err := e.store.InsertWorkflow(t.Context(), row, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	poll := func() chan struct{} {
		done := make(chan struct{})
		go func() { e.srv.pollWorkflows(context.Background()); close(done) }()
		return done
	}
	done := poll()
	req := agentRecv(t, agent)
	p, err := protocol.DecodePayload[protocol.WorkflowControl](req)
	if err != nil || p.Action != "start" || p.UserID != owner || p.Workflow.Cwd != "/project" {
		t.Fatal(p, err)
	}
	agentReply(t, agent, req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &protocol.Workflow{ID: row.ID, State: "running", Revision: 2}})
	<-done
	done = poll()
	req = agentRecv(t, agent)
	p, err = protocol.DecodePayload[protocol.WorkflowControl](req)
	if err != nil || p.Action != "inspect" {
		t.Fatal("work was replayed", p, err)
	}
	path := "/api/v1/devices/" + device + "/workflows/" + row.ID + "/cancel"
	r := e.post(t, path, token, nil)
	if r.Status != 200 {
		t.Fatal(r.Status)
	}
	agentReply(t, agent, req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &protocol.Workflow{ID: row.ID, State: "running", Revision: 3}})
	<-done
	saved, _ := e.store.Workflow(t.Context(), row.ID)
	if saved.State != "cancelling" {
		t.Fatal("late running response undid cancellation")
	}
	done = poll()
	req = agentRecv(t, agent)
	p, _ = protocol.DecodePayload[protocol.WorkflowControl](req)
	if p.Action != "cancel" {
		t.Fatal(p.Action)
	}
	agentReply(t, agent, req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &protocol.Workflow{ID: row.ID, State: "cancelled", Revision: 4}})
	<-done
	ac, _ := e.srv.reg.Agent(device)
	e.srv.applyWorkflow(ac, &protocol.Workflow{ID: row.ID, State: "running", Revision: 99})
	saved, _ = e.store.Workflow(t.Context(), row.ID)
	if saved.State != "cancelled" {
		t.Fatal("terminal state regressed")
	}
}
