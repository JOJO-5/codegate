/** Match a launched CLI, including Windows npm shims; never guess from a title. */
export function sessionQuotaProvider(command: string, args: string[] = []): string {
  const base = (value: string): string => value.replace(/\\/g, '/').split('/').at(-1)!.toLowerCase().replace(/\.(exe|cmd|bat)$/, '')
  let name = base(command)
  if (name === 'cmd') {
    const index = args.findIndex(arg => arg.toLowerCase() === '/c')
    name = index >= 0 && args[index + 1] ? base(args[index + 1]!) : ''
  }
  return ['codex', 'claude', 'opencode'].includes(name) ? name : ''
}
