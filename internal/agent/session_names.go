package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Called under workspaceMu, shared by creation and recovery.
func (a *Agent) sessionName(explicit, cwd, label string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	project := filepath.Base(filepath.Clean(cwd))
	if project == "." || project == string(filepath.Separator) {
		project = "工作区"
	}
	base := project + " · " + label
	path := filepath.Join(a.cfg.StateDir, "session-name-counters.json")
	counters := map[string]int{}
	b, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(b, &counters); err != nil {
			return "", fmt.Errorf("读取会话编号: %w", err)
		}
		if counters == nil {
			counters = map[string]int{}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	used := map[string]bool{}
	for _, s := range a.mgr.List() {
		used[s.Summary().Name] = true
	}
	n := counters[base] + 1
	name := numberedSessionName(base, n)
	for used[name] {
		n++
		name = numberedSessionName(base, n)
	}
	counters[base] = n
	if err := writeLiveJSON(path, counters); err != nil {
		return "", fmt.Errorf("保存会话编号: %w", err)
	}
	return name, nil
}

func numberedSessionName(base string, n int) string {
	if n == 1 {
		return base
	}
	return fmt.Sprintf("%s %d", base, n)
}
