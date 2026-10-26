/**
 * 预置命令表与命令分类。
 *
 * 新 Agent 会通过心跳上报本机已配置与已安装命令；旧 Agent 仍使用
 * 这份预置表作为兼容回退。检测结果不会授予命令执行权限。
 *
 * 分类用于模板和 Shell 输入方式；Ctrl+C 的防误退策略独立判断编程 CLI
 * 的实际身份，不能根据可修改的会话名称或 kind 推断底层中断能力。
 * Windows 交互式 cmd/PowerShell 已补充真实中断及继续执行的验证，
 * 见 docs/CTRL-C-POLICY-2026-10-26.md。
 */

/** 命令的分类。 */
export type CommandKind = 'shell' | 'tui'

export interface CommandPreset {
  /** 发给服务端的 `command_id`。 */
  id: string
  /** 界面上显示的名字。 */
  label: string
  /** 一句话说明。 */
  hint: string
  kind: CommandKind
  /** 是否是「恢复上次对话」（§21.3）。 */
  resume?: boolean
}

/**
 * 预置命令。
 *
 * 只列 MVP 的主用例：AI 编码 CLI + 一个 shell 兜底。
 * 刻意不列 vim/htop —— 它们是 tui，行为上没问题，但用手机软键盘操作
 * 全屏 TUI 体验很差，列出来只会让人试一次然后放弃。
 */
export const PRESETS: CommandPreset[] = [
  {
    id: 'claude',
    label: 'Claude Code',
    hint: 'Anthropic 官方 CLI',
    kind: 'tui',
  },
  {
    id: 'codex',
    label: 'Codex CLI',
    hint: 'OpenAI Codex',
    kind: 'tui',
  },
  {
    id: 'opencode',
    label: 'OpenCode',
    hint: '开源终端编码助手',
    kind: 'tui',
  },
  {
    id: 'dsh',
    label: 'DeepSeek Harness',
    hint: '需安装并验证 TUI 插件（如 turtle-ui）',
    kind: 'tui',
  },
  {
    id: 'shell',
    label: 'Shell',
    hint: 'cmd / PowerShell 等命令行终端',
    kind: 'shell',
  },
]

/**
 * 已知的交互式 Shell；未知命令按 tui 处理，但这不决定 Ctrl+C 策略。
 */
const SHELL_COMMANDS = new Set([
  'cmd',
  'cmd.exe',
  'powershell',
  'powershell.exe',
  'pwsh',
  'pwsh.exe',
  'bash',
  'bash.exe',
  'sh',
  'zsh',
  'fish',
  'nu',
  'nushell',
  'wsl',
  'wsl.exe',
])

/**
 * 从一个命令行字符串判断它的类别。
 *
 * 输入可能是 `/bin/bash -l`、`C:\Windows\System32\cmd.exe /k`、`claude --continue`
 * 这类形式，所以要取 basename 并剥掉扩展名再比。
 *
 * 未知命令返回 `'tui'`，这是命令类别的启发式回退，不是执行授权或
 * 中断能力证明。调用方应优先使用配置及原生 CLI 的可靠身份。
 */
export function classifyCommand(commandLine: string): CommandKind {
  const first = commandLine.trim().split(/\s+/)[0] ?? ''
  if (first === '') return 'tui'

  // 取 basename：同时处理 / 和 \ 两种分隔符（前端可能收到任一平台的路径）
  const base = first.split(/[/\\]/).pop() ?? first
  const lower = base.toLowerCase()

  if (SHELL_COMMANDS.has(lower)) return 'shell'
  // 去掉 .exe / .cmd / .bat 再试一次
  const stripped = lower.replace(/\.(exe|cmd|bat|ps1)$/, '')
  if (SHELL_COMMANDS.has(stripped)) return 'shell'

  return 'tui'
}

/** 取预设；找不到返回 undefined（调用方决定怎么降级）。 */
export function presetById(id: string): CommandPreset | undefined {
  return PRESETS.find((p) => p.id === id)
}
