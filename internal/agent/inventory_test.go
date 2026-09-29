package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandInventoryNeverAuthorizesDiscoveredTool(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "custom")
	if err := os.WriteFile(bin, []byte("test"), 0755); err != nil { t.Fatal(err) }
	a := &Agent{cfg: Config{AllowedCommands: []CommandSpec{{ID:"custom", Label:"Custom", Command:bin, Kind:KindTUI, WebURL:"https://example.test/dsh"}}}}
	commands := a.commandInventory()
	if len(commands) == 0 || commands[0].ID != "custom" || !commands[0].Installed || !commands[0].Allowed ||
		commands[0].WebURL != "https://example.test/dsh" { t.Fatalf("configured command missing: %+v", commands) }
	for _, item := range commands[1:] {
		if item.Allowed { t.Errorf("discovered %q was incorrectly authorized", item.ID) }
	}
	if err := os.Remove(bin); err != nil { t.Fatal(err) }
	if commandInstalled(bin) { t.Fatal("missing binary reported installed") }
}

func TestCommandWebURLRequiresHTTPS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ServerURL = "wss://example.test/api/v1/ws/agent"
	cfg.AllowedRoots = []string{t.TempDir()}
	cfg.AllowedCommands = []CommandSpec{{ID:"dsh", Label:"DSH", Command:"dsh", Kind:KindTUI, WebURL:"http://127.0.0.1:3080"}}
	if err := cfg.Validate(); err == nil { t.Fatal("accepted insecure web URL") }
	cfg.AllowedCommands[0].WebURL = "https://dsh.example.test/"
	if err := cfg.Validate(); err != nil { t.Fatal(err) }
}
