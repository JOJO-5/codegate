package agent

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
)

var ErrNativeLaunchMode = errors.New("所选 CLI 原生启动模式不可用；请刷新工具列表或选择继承本机设置")
var ErrNativeLaunchVersion = errors.New("本机 Codex 版本不能确认支持所选原生启动参数；请升级至 0.164 或更新版本，或选择继承本机设置")

type nativeLaunchChoice struct {
	protocol.NativeLaunchMode
	args []string
}

// These are CLI-owned TUI modes expressed as native argv. Missing native modes
// must not be emulated with custom permission rules, plugins or prompts.
func nativeLaunchChoices(spec CommandSpec) ([]nativeLaunchChoice, string) {
	cmd := ResolvedCommand{Command: spec.Command, Args: spec.Args}
	tool := nativeTool(cmd)
	args := spec.Args
	if strings.EqualFold(filepath.Base(spec.Command), "cmd.exe") && len(args) >= 2 && strings.EqualFold(args[0], "/c") {
		args = args[2:]
	}
	if tool == "dsh" && len(args) == 2 && args[0] == "--profile" && args[1] == "tui" {
		args = nil
	}
	if spec.Kind != KindTUI || len(args) != 0 {
		return nil, "此命令已有本机固定参数；请在 CLI 原生界面中选择权限模式。"
	}
	var out []nativeLaunchChoice
	add := func(id, label, description string, full bool, args ...string) {
		out = append(out, nativeLaunchChoice{NativeLaunchMode: protocol.NativeLaunchMode{ID: tool + "." + id, Label: label, Description: description, FullAccess: full, CommandHint: strings.Join(args, " ")}, args: args})
	}
	switch tool {
	case "codex":
		add("read-only", "只读（Codex 原生）", "使用 Codex 原生只读命令沙箱；需要额外权限时由 Codex 询问。", false, "-c", "default_permissions=:read-only", "-a", "on-request")
		add("workspace", "工作区读写（Codex 原生）", "使用 Codex 原生工作区读写模式，额外权限由 Codex 询问。", false, "-c", "default_permissions=:workspace", "-a", "on-request")
		add("auto", "自动审批（Codex 原生）", "使用 Codex 的自动审批功能；是否可用及审批行为由本机 CLI 决定。", false, "-c", "default_permissions=:workspace", "--approve-for-me")
		add("full-access", "完全权限（Codex 原生）", "关闭 Codex 命令沙箱并不再询问审批。CLI 可操作工作目录以外的文件；仅本次启动使用。", true, "-c", "default_permissions=:danger-full-access", "-a", "never")
	case "claude":
		for _, m := range []struct {
			id, label, mode, description string
			full                         bool
		}{
			{"manual", "手动审批（Claude 原生）", "manual", "按 Claude 原生规则询问权限。", false},
			{"plan", "计划模式（Claude 原生）", "plan", "使用 Claude 原生 Plan 模式；工具限制由 Claude 处理。", false},
			{"accept-edits", "自动接受编辑（Claude 原生）", "acceptEdits", "自动接受文件编辑，其他操作按 Claude 原生权限规则处理。", false},
			{"auto", "自动模式（Claude 原生）", "auto", "使用 Claude 原生自动审批；可用性由本机版本、模型和账号决定。", false},
			{"dont-ask", "不询问（Claude 原生）", "dontAsk", "不弹出权限询问，原生规则未允许的操作会被拒绝。", false},
			{"full-access", "完全权限（Claude 原生）", "bypassPermissions", "跳过 Claude 权限审批。CLI 可操作工作目录以外的文件；仅本次启动使用。", true},
		} {
			add(m.id, m.label, m.description, m.full, "--permission-mode", m.mode)
		}
	case "opencode":
		add("auto", "自动批准（OpenCode 原生）", "使用 OpenCode 2 的 --auto；自动批准未被原生规则明确拒绝的权限请求。", false, "--auto")
		return out, "OpenCode 2 支持原生自动批准预选；其他权限设置仍在 OpenCode 自己的界面中处理。"
	case "dsh":
		return nil, "当前集成暂不支持从启动参数选择 DSH 权限模式；请在 DSH 原生界面中选择。"
	default:
		return nil, "当前工具暂不支持原生权限模式预选；请在工具界面中选择。"
	}
	return out, ""
}

func nativeLaunchInventory(spec CommandSpec) ([]protocol.NativeLaunchMode, string) {
	choices, note := nativeLaunchChoices(spec)
	var out []protocol.NativeLaunchMode
	for _, choice := range choices {
		out = append(out, choice.NativeLaunchMode)
	}
	return out, note
}

func (c Config) applyNativeLaunchMode(commandID, mode string, cmd *ResolvedCommand) error {
	if mode == "" {
		return nil
	}
	if spec, ok := c.FindCommand(commandID); ok {
		choices, _ := nativeLaunchChoices(spec)
		for _, choice := range choices {
			if choice.ID == mode {
				cmd.Args = append(cmd.Args, choice.args...)
				cmd.NativeLaunchMode = mode
				return nil
			}
		}
	}
	return ErrNativeLaunchMode
}

func checkCodexNativeLaunchVersion(ctx context.Context, c ResolvedCommand, env []string, cwd string) error {
	if c.NativeLaunchMode == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{}
	if strings.EqualFold(filepath.Base(c.Command), "cmd.exe") && len(c.Args) >= 2 && strings.EqualFold(c.Args[0], "/c") {
		args = append(args, c.Args[:2]...)
	}
	cmd := exec.CommandContext(ctx, c.Command, append(args, "--version")...)
	cmd.Env = env
	cmd.Dir = cwd
	processutil.Background(cmd)
	out, err := cmd.Output()
	if err != nil || len(out) > 512 {
		return ErrNativeLaunchVersion
	}
	parts := regexp.MustCompile(`codex-cli (\d+)\.(\d+)\.`).FindStringSubmatch(string(out))
	if len(parts) != 3 {
		return ErrNativeLaunchVersion
	}
	major, _ := strconv.Atoi(parts[1])
	minor, _ := strconv.Atoi(parts[2])
	if major == 0 && minor < 164 {
		return ErrNativeLaunchVersion
	}
	return nil
}
