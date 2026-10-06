package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/klauspost/compress/zstd"
)

func TestNativeHistorySameDirectoryAndZstd(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	other := t.TempDir()
	key := uuid.NewString()
	a := &Agent{cfg: Config{StateDir: root}}
	store, e := a.privateDSHRoot(key)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(store, 0700); e != nil {
		t.Fatal(e)
	}
	encoder, e := zstd.NewWriter(nil)
	if e != nil {
		t.Fatal(e)
	}
	defer encoder.Close()
	good := uuid.NewString()
	for i, d := range []string{cwd, other} {
		id := good
		if i > 0 {
			id = uuid.NewString()
		}
		header, _ := json.Marshal(map[string]any{"type": "session", "version": 4, "id": id, "cwd": d, "createdAt": 123, "delegationDepth": 0})
		if e = os.WriteFile(filepath.Join(store, id+".jsonl.zstd"), encoder.EncodeAll(append(header, '\n'), nil), 0600); e != nil {
			t.Fatal(e)
		}
	}
	history, e := a.nativeHistory("dsh", cwd, nil, key, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(history) != 1 || history[0].ID != good {
		t.Fatalf("cross-directory history: %+v", history)
	}
	if _, e = a.nativeHistory("dsh", cwd, nil, "../../outside", ""); e == nil {
		t.Fatal("accepted traversal key")
	}
	if _, e = a.nativeHistory("dsh", cwd, nil, key, "--last"); e == nil {
		t.Fatal("accepted option injection")
	}
}
func TestRecoveryRejectsChangedOrUnapprovedCommand(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCommands: []CommandSpec{{ID: "codex", Command: "codex"}}}}
	source := protocol.SessionSummary{Command: "codex", Args: []string{"exec", "malicious"}}
	if _, _, e := a.recoveryCommand(source); e == nil {
		t.Fatal("accepted changed legacy command")
	}
	source.Recovery = &protocol.ConversationBinding{CommandID: "unknown"}
	if _, _, e := a.recoveryCommand(source); e == nil {
		t.Fatal("accepted unapproved command")
	}
}
func TestCodexCallbackStoresOnlyIdentity(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "identity.json")
	id := uuid.NewString()
	raw := `{"thread-id":"` + id + `","cwd":"/workspace","last-assistant-message":"private","input-messages":["secret"]}`
	if e := RecordConversation([]string{marker, "[]", raw}); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(marker)
	if e != nil {
		t.Fatal(e)
	}
	var actual map[string]string
	if e = json.Unmarshal(data, &actual); e != nil {
		t.Fatal(e)
	}
	if len(actual) != 2 || actual["id"] != id || actual["cwd"] != "/workspace" {
		t.Fatalf("unexpected private data: %v", actual)
	}
}
