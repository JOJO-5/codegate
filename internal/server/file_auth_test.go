package server

import (
	"testing"

	"github.com/jojo/codegate/internal/protocol"
)

func TestSessionIDOfRejectsEnvelopePayloadMismatch(t *testing.T) {
	env, err := protocol.NewRequest("request", protocol.TypeFileRead,
		"11111111-1111-4111-8111-111111111111",
		protocol.FileReadPayload{SessionID: "22222222-2222-4222-8222-222222222222", Path: "file.txt", Length: 1})
	if err != nil { t.Fatal(err) }
	if got := sessionIDOf(env); got != "" {
		t.Fatalf("authorized session must match executed session, got %q", got)
	}
	env.SessionID = ""
	if got := sessionIDOf(env); got != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("payload session ID = %q", got)
	}
}
