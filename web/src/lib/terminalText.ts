import type { IBuffer } from '@xterm/xterm'

export interface TerminalTextSnapshot {
  text: string
  viewportOffset: number
}

/** Freeze the displayed buffer and join soft wraps without inserting newlines. */
export function terminalTextSnapshot(buffer: IBuffer): TerminalTextSnapshot {
  let text = ''
  let viewportOffset = 0
  for (let row = 0; row < buffer.length; row++) {
    const line = buffer.getLine(row)
    if (!line) continue
    if (row > 0 && !line.isWrapped) text += '\n'
    if (row === buffer.viewportY) viewportOffset = text.length
    text += line.translateToString(!buffer.getLine(row + 1)?.isWrapped)
  }
  return { text, viewportOffset }
}
