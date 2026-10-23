package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

func TestSessionMetadataAvailableWithoutLiveTerminal(t *testing.T) {
	e := newTestEnv(t)
	alice := e.signup(t, "metadata-alice@example.com", "correct-horse-battery")
	bob := e.signup(t, "metadata-bob@example.com", "correct-horse-battery")
	owner := e.meUserID(t, alice)
	device, _ := e.seedPairedDevice(t, owner)
	sid := uuid.NewString()
	meta := &storage.SessionMeta{ID: sid, DeviceID: device, UserID: owner,
		Command: "codex", Name: "Closed conversation", Cwd: "/allowed/project", Status: "terminated",
		Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: uuid.NewString()}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sessions/" + sid
	got := e.get(t, path, alice)
	if got.Status != http.StatusOK {
		t.Fatalf("closed/offline metadata unavailable: %d %s", got.Status, got.Body)
	}
	var summary protocol.SessionSummary
	if err := json.Unmarshal(got.Body, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.SessionID != sid || summary.DeviceID != device || summary.Status != "terminated" || summary.Recovery.NativeID != meta.Recovery.NativeID {
		t.Fatalf("recovery context lost: %+v", summary)
	}
	foreign := e.get(t, path, bob)
	missing := e.get(t, "/api/v1/sessions/"+uuid.NewString(), bob)
	if foreign.Status != missing.Status || foreign.ErrorCode(t) != missing.ErrorCode(t) || string(foreign.Body) != string(missing.Body) {
		t.Fatal("metadata endpoint exposes another owner's session existence")
	}
	if e.get(t, path, "").Status != http.StatusUnauthorized {
		t.Fatal("unauthenticated metadata request accepted")
	}
}
