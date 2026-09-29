/**
 * 预置命令表与命令分类。
 *
 * 新 Agent 会通过心跳上报本机已配置与已安装命令；旧 Agent 仍使用
 * 这份预置表作为兼容回退。检测结果不会授予命令执行权限。
 *
 * # 分类为什么重要
 *
 * 它直接决定 Ctrl+C 按钮的可用性，见 `docs/PHASE0-ARCHITECTURE.md` §6.5：
 *
 *   - **tui**（raw mode 应用）：Ctrl+C 写 0x03 能真正中断进程 ✅
 *   - **shell**（cooked mode）：0x03 会被 conhost 拦截，子进程只收到 stdin EOF，
 *     命令**不会中断** ❌ —— 系统自带的 ConPTY 没有可靠办法做到这件事
 *
 * 所以对 shell 类命令，界面的责任是**如实告知并给替代路径**，
 * 而不是放一个点了没反应的按钮。那比没有按钮更糟。
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
    hint: 'cmd / PowerShell —— 注意 Ctrl+C 在 Windows 上无法中断它',
    kind: 'shell',
  },
]

/**
 * 已知的 **cooked mode** 程序（即 Ctrl+C 无效的那些）。
 *
 * 只列确定是 shell 的：多列一个不会造成伤害（只是把按钮置灰），
 * 但漏列一个会让用户以为 Ctrl+C 坏了。所以这个名单要**保守地小**，
 * 未知的一律按 tui 处理（见 `classifyCommand`）。
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
 * ★ 未知命令返回 `'tui'`。这个默认值是刻意选的：
 *   主用例（Claude Code / Codex / OpenCode）都是 tui，
 *   把它们误判成 shell 会让主功能"看起来坏了"；
 *   反过来把一个未知程序误判成 tui，最坏结果是 Ctrl+C 点了没反应 ——
 *   而我们始终提供「终止会话」作为兜底动作，所以这个错误是可恢复的。
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
