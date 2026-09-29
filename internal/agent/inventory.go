package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/jojo/codegate/internal/protocol"
)

// Known tools are discovered for display. Only allowed_commands can be run.
var knownCLIs = []struct{ id, label, binary string }{
	{"claude", "Claude Code", "claude"},
	{"codex", "Codex CLI", "codex"},
	{"opencode", "OpenCode", "opencode"},
	{"dsh", "DeepSeek Harness", "dsh"},
}

func commandInstalled(command string) bool {
	if filepath.IsAbs(command) {
		info, err := os.Stat(command)
		return err == nil && info.Mode().IsRegular() &&
			(runtime.GOOS == "windows" || info.Mode()&0111 != 0)
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func (a *Agent) commandInventory() []protocol.CommandAvailability {
	out := make([]protocol.CommandAvailability, 0, len(knownCLIs)+len(a.cfg.AllowedCommands))
	seen := make(map[string]bool)
	for _, spec := range a.cfg.AllowedCommands {
		out = append(out, protocol.CommandAvailability{
			ID: spec.ID, Label: spec.Label, Kind: string(spec.Kind),
			Installed: commandInstalled(spec.Command), Allowed: true,
			Resume: len(spec.ResumeArgs) > 0, WebURL: spec.WebURL,
		})
		seen[spec.ID] = true
	}
	for _, candidate := range knownCLIs {
		if seen[candidate.id] { continue }
		out = append(out, protocol.CommandAvailability{
			ID: candidate.id, Label: candidate.label, Kind: string(KindTUI),
			Installed: commandInstalled(candidate.binary),
		})
	}
	return out
}
