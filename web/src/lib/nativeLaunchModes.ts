import type { NativeLaunchMode } from './protocol'

export function launchModeIssue(modes: NativeLaunchMode[] | undefined, id: string): string {
  if (!id) return ''
  return modes?.some(mode => mode.id === id) ? '' : '所选原生启动模式已不可用，请重新选择；不会自动改用其他权限。'
}
