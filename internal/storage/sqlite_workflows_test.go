package storage

import (
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"testing"
)

func TestWorkflowOwnershipNonceRevisionAndRefresh(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	u := seedUser(t, s, "workflow@example.test")
	d := seedDevice(t, s, u.ID)
	row := &protocol.Workflow{ID: uuid.NewString(), UserID: u.ID, DeviceID: d.ID, State: "queued", Revision: 1, Steps: []protocol.WorkflowStep{}}
	nonce := uuid.NewString()
	if err := s.InsertWorkflow(ctx, row, nonce); err != nil {
		t.Fatal(err)
	}
	got, err := s.WorkflowByNonce(ctx, u.ID, nonce)
	if err != nil || got.ID != row.ID || got.UserID != u.ID {
		t.Fatalf("persistent owner/nonce: %+v %v", got, err)
	}
	if err = s.InsertWorkflow(ctx, row, nonce); err == nil {
		t.Fatal("duplicate request accepted")
	}
	list, err := s.Workflows(ctx, "other", d.ID)
	if err != nil || len(list) != 0 {
		t.Fatal("cross-account workflow leak")
	}
	row.State = "cancelling"
	row.Revision = 2
	if ok, err := s.UpdateWorkflow(ctx, row, 1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	row.State = "running"
	row.Revision = 3
	if ok, err := s.UpdateWorkflow(ctx, row, 1); err != nil || ok {
		t.Fatal("stale result overwrote cancellation")
	}
	got, err = s.Workflow(ctx, row.ID)
	if err != nil || got.State != "cancelling" {
		t.Fatal("refresh lost cancellation")
	}
}
