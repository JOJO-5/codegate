package storage

import (
	"context"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"testing"
)

func TestContinuationQueueOwnershipNonceAndCompare(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "tasks@example.com")
	d := seedDevice(t, s, u.ID)
	row := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: u.ID, DeviceID: d.ID, State: "queued", DueAt: 100, CreatedAt: 10}
	nonce := uuid.NewString()
	if e := s.InsertContinuation(ctx, row, nonce); e != nil {
		t.Fatal(e)
	}
	got, e := s.ContinuationByNonce(ctx, u.ID, nonce)
	if e != nil || got.UserID != u.ID || got.ID != row.ID {
		t.Fatal(got, e)
	}
	if e = s.InsertContinuation(ctx, row, nonce); e == nil {
		t.Fatal("duplicate submission")
	}
	list, e := s.Continuations(ctx, "other", d.ID)
	if e != nil || len(list) != 0 {
		t.Fatal("owner leak")
	}
	ready, e := s.ReadyContinuations(ctx, 99)
	if e != nil || len(ready) != 0 {
		t.Fatal(ready, e)
	}
	ready, e = s.ReadyContinuations(ctx, 100)
	if e != nil || len(ready) != 1 {
		t.Fatal(ready, e)
	}
	row.State = "cancelled"
	ok, e := s.UpdateContinuation(ctx, row, "queued")
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	row.State = "running"
	ok, e = s.UpdateContinuation(ctx, row, "queued")
	if e != nil || ok {
		t.Fatal("late worker overwrote cancellation")
	}
}
