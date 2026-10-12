package agent

import (
	"errors"
	"strings"
)

// Approved CLI arguments are retained. UI-only switches are removed; prompts
// are always written to stdin to avoid shell/npm-shim argument interpolation.
func continuationArgs(c ResolvedCommand, id string) ([]string, error) {
	tool := quotaProvider(c.Command, c.Args)
	base := append([]string{}, c.Args...)
	prefix := []string{}
	if strings.HasSuffix(strings.ToLower(c.Command), "cmd.exe") && len(base) >= 2 && strings.EqualFold(base[0], "/c") {
		prefix = base[:2]
		base = base[2:]
	}
	filtered := []string{}
	for _, arg := range base {
		if arg == "--no-alt-screen" {
			continue
		}
		if arg == "--ephemeral" || arg == "--fork" || arg == "--fork-session" || arg == "--last" || arg == "resume" || arg == "exec" || arg == "run" || arg == "--continue" {
			return nil, errors.New("当前CLI配置包含会话/运行模式参数，请使用普通CLI配置安排原对话续跑")
		}
		filtered = append(filtered, arg)
	}
	args := append(prefix, filtered...)
	switch tool {
	case "codex":
		return append(args, "exec", "resume", "--json", "--skip-git-repo-check", id, "-"), nil
	case "claude":
		return append(args, "--print", "--resume", id, "--output-format", "json"), nil
	case "opencode":
		return append(args, "run", "--session", id, "--format", "json"), nil
	default:
		return nil, errors.New("此CLI暂不支持后台原对话续跑")
	}
}

func continuationQuotaEligible(c ResolvedCommand) bool {
	args := c.Args
	if strings.HasSuffix(strings.ToLower(c.Command), "cmd.exe") && len(args) >= 2 && strings.EqualFold(args[0], "/c") {
		args = args[2:]
	}
	return len(args) == 0
}
