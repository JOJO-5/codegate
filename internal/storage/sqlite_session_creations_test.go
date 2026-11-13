package storage

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestSessionCreationReservationSurvivesReopenAndCompetingRequests(t *testing.T) {
	s := newTestStore(t)
	u := seedUser(t, s, "creation-store@example.com")
	d := seedDevice(t, s, u.ID)
	intent := &SessionCreation{UserID: u.ID, DeviceID: d.ID, Nonce: uuid.NewString(), RequestID: uuid.NewString(), Fingerprint: "original", CreatedAt: 1}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ReserveSessionCreation(t.Context(), intent)
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reserved %d times", count)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(t.Context(), SQLiteOptions{Path: s.path, Logger: s.log})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.SessionCreation(t.Context(), u.ID, d.ID, intent.Nonce)
	if err != nil || got.Result != nil || got.Fingerprint != "original" {
		t.Fatal("pending reservation lost after restart", got, err)
	}
	if ok, err := reopened.ReserveSessionCreation(t.Context(), intent); err != nil || ok {
		t.Fatal("restart allowed duplicate dispatch", err)
	}
	result, _ := protocol.NewEnvelope(protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: protocol.SessionSummary{SessionID: uuid.NewString()}})
	if err := reopened.ResolveSessionCreation(t.Context(), u.ID, d.ID, intent.RequestID, result); err != nil {
		t.Fatal(err)
	}
	late := protocol.NewErrorEnvelope(intent.RequestID, protocol.NewError(protocol.CodeInternal, "late failure"))
	_ = reopened.ResolveSessionCreation(t.Context(), u.ID, d.ID, intent.RequestID, late)
	got, err = reopened.SessionCreation(t.Context(), u.ID, d.ID, intent.Nonce)
	if err != nil || got.Result.Type != protocol.TypeSessionCreated {
		t.Fatal("late response replaced durable receipt", err)
	}
	if _, err = reopened.SessionCreation(t.Context(), "other", d.ID, intent.Nonce); err != ErrNotFound {
		t.Fatal("cross-account receipt", err)
	}
}
