package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApprovedToolsOnlyExtendKnownCommands(t *testing.T) {
	a := &Agent{cfg: Config{}, approvedTools: map[string]bool{"codex": true, "unknown": true}}
	cfg := a.commandConfig()
	if _, ok := cfg.FindCommand("codex"); !ok {
		t.Fatal("approved Codex missing")
	}
	if _, ok := cfg.FindCommand("unknown"); ok {
		t.Fatal("unknown command granted")
	}
	if _, err := cfg.ResolveCommand("unknown", "", nil, false); err == nil {
		t.Fatal("unknown command executed")
	}
	if _, err := cfg.ResolveCommand("codex", "", nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedToolsRejectUnknownPersistedID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "approved-tools.json"), []byte(`["unknown"]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApprovedTools(dir); err == nil {
		t.Fatal("unknown persisted ID accepted")
	}
}

func TestWebApprovalDoesNotGrantTerminalCommand(t *testing.T) {
	a := &Agent{approvedTools: map[string]bool{"dsh-web": true}}
	if !a.dshWebEnabled() {
		t.Fatal("web approval ignored")
	}
	if _, ok := a.commandConfig().FindCommand("dsh-web"); ok {
		t.Fatal("web approval allowed arbitrary terminal invocation")
	}
	if _, ok := a.commandConfig().FindCommand("dsh"); ok {
		t.Fatal("web approval allowed DSH TUI")
	}
}
func TestWebAuthorityWithPort(t *testing.T) {
	for _, host := range []string{"codegate.example.test:8443", "device.dsh.example.test"} {
		if !validWebAuthority(host) {
			t.Fatal(host)
		}
	}
	for _, host := range []string{"codegate.example.test:0", "codegate.example.test:65536", "codegate.example.test:8443/path", "user@codegate.example.test"} {
		if validWebAuthority(host) {
			t.Fatal(host)
		}
	}
}
