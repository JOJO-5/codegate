package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/jojo/codegate/internal/protocol"
)

// Browser approvals are limited to this fixed inventory. The local config
// remains the authority for roots, arbitrary commands, and DSH Web access.
func knownTool(id string) (CommandSpec, bool) {
	for _, tool := range knownCLIs {
		if tool.id != id { continue }
		spec := CommandSpec{ID: id, Label: tool.label, Command: tool.binary, Kind: KindTUI}
		if id == "dsh" { spec.Args = []string{"--profile", "tui"} }
		if runtime.GOOS == "windows" {
			spec.Command = "cmd.exe"
			spec.Args = append([]string{"/c", tool.binary}, spec.Args...)
		}
		return spec, true
	}
	return CommandSpec{}, false
}

func approvedToolsPath(stateDir string) string { return filepath.Join(stateDir, "approved-tools.json") }

func loadApprovedTools(stateDir string) (map[string]bool, error) {
	approved := make(map[string]bool)
	data, err := os.ReadFile(approvedToolsPath(stateDir))
	if errors.Is(err, os.ErrNotExist) { return approved, nil }
	if err != nil { return nil, fmt.Errorf("读取工具授权失败: %w", err) }
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil { return nil, fmt.Errorf("解析工具授权失败: %w", err) }
	for _, id := range ids {
		if _, ok := knownTool(id); !ok { return nil, fmt.Errorf("工具授权包含未知 ID: %q", id) }
		approved[id] = true
	}
	return approved, nil
}

func (a *Agent) commandConfig() Config {
	a.toolMu.RLock()
	defer a.toolMu.RUnlock()
	cfg := a.cfg
	cfg.AllowedCommands = append([]CommandSpec(nil), cfg.AllowedCommands...)
	for _, tool := range knownCLIs {
		if !a.approvedTools[tool.id] { continue }
		if _, exists := cfg.FindCommand(tool.id); exists { continue }
		spec, _ := knownTool(tool.id)
		cfg.AllowedCommands = append(cfg.AllowedCommands, spec)
	}
	return cfg
}

func (a *Agent) onToolSet(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.ToolSetPayload](req)
	if err != nil { a.replyError(req, err); return }
	_, ok := knownTool(p.ID)
	if !ok { a.replyError(req, errors.New("只能授权内置工具")); return }
	if p.Enabled {
		if _, err := exec.LookPath(p.ID); err != nil {
			a.replyError(req, errors.New("Agent 服务账户未找到该工具")); return
		}
	}
	a.toolMu.Lock()
	next := make(map[string]bool, len(a.approvedTools)+1)
	for id, enabled := range a.approvedTools { if enabled { next[id] = true } }
	if p.Enabled { next[p.ID] = true } else { delete(next, p.ID) }
	ids := make([]string, 0, len(next))
	for _, candidate := range knownCLIs { if next[candidate.id] { ids = append(ids, candidate.id) } }
	data, err := json.Marshal(ids)
	if err == nil { err = os.MkdirAll(a.cfg.StateDir, 0700) }
	if err == nil {
		var tmp *os.File
		tmp, err = os.CreateTemp(a.cfg.StateDir, ".approved-tools-*")
		if err == nil {
			defer os.Remove(tmp.Name())
			if err = tmp.Chmod(0600); err == nil { _, err = tmp.Write(data) }
			if closeErr := tmp.Close(); err == nil { err = closeErr }
			if err == nil { err = os.Rename(tmp.Name(), approvedToolsPath(a.cfg.StateDir)) }
		}
	}
	if err == nil { a.approvedTools = next }
	a.toolMu.Unlock()
	if err != nil { a.replyError(req, fmt.Errorf("保存工具授权失败: %w", err)); return }
	_ = a.sendHeartbeat(context.Background())
	a.reply(req, protocol.TypeToolUpdated, p)
}
