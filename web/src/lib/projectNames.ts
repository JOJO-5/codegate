export function validProjectName(name: string): boolean {
  if (!name || name === '.' || name === '..' || new TextEncoder().encode(name).length > 255 || name.trim() !== name || name.endsWith('.') || /[<>:"/\\|?*\x00-\x1f]/.test(name)) return false
  return !/^(CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])(?:\.|$)/i.test(name)
}
