package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jojo/codegate/internal/processutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var claudeVersionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

func claudeSupportsLiveHooks(c ResolvedCommand, env []string) bool {
	args := []string{"--version"}
	if strings.HasSuffix(strings.ToLower(c.Command), "cmd.exe") && len(c.Args) >= 2 {
		args = append(append([]string{}, c.Args[:2]...), args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Command, args...)
	cmd.Env = env
	processutil.Background(cmd)
	out, e := cmd.Output()
	if e != nil {
		return false
	}
	m := claudeVersionPattern.FindStringSubmatch(string(out))
	if len(m) != 4 {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return major > 2 || major == 2 && (minor > 1 || minor == 1 && patch >= 296)
}
func injectClaudeLiveHooks(args []string, marker, exe string) ([]string, error) {
	settings := map[string]any{}
	kept := []string{}
	for i := 0; i < len(args); i++ {
		value := ""
		found := false
		if args[i] == "--settings" {
			if i+1 == len(args) {
				return nil, errors.New("Claude settings缺少参数")
			}
			i++
			value = args[i]
			found = true
		} else if strings.HasPrefix(args[i], "--settings=") {
			value = strings.TrimPrefix(args[i], "--settings=")
			found = true
		}
		if !found {
			kept = append(kept, args[i])
			continue
		}
		var b []byte
		if strings.HasPrefix(strings.TrimSpace(value), "{") {
			b = []byte(value)
		} else {
			st, e := os.Stat(value)
			if e != nil || st.Size() > 1<<20 {
				return nil, errors.New("无法读取原Claude settings")
			}
			var e2 error
			b, e2 = os.ReadFile(value)
			if e2 != nil {
				return nil, e2
			}
		}
		var overlay map[string]any
		if json.Unmarshal(b, &overlay) != nil {
			return nil, errors.New("原Claude settings不是有效JSON")
		}
		for k, v := range overlay {
			settings[k] = v
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	command := quoteHookCommand(exe) + " record-live-event " + quoteHookCommand(marker)
	for _, event := range []string{"UserPromptSubmit", "Stop", "StopFailure"} {
		old, _ := hooks[event].([]any)
		hooks[event] = append(old, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 5}}})
	}
	settings["hooks"] = hooks
	path := marker + ".settings.json"
	if e := writeLiveJSON(path, settings); e != nil {
		return nil, e
	}
	return append(kept, "--settings", path), nil
}
