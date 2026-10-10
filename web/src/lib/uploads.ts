import type { useConnStore } from '../stores/conn'
import { MessageType } from './protocol'
import { TransferChunkSizer } from './transferPacing'

export const MAX_UPLOAD_SIZE = 100 * 1024 * 1024
type Connection = Pick<ReturnType<typeof useConnStore>, 'isOpen' | 'request'>
export interface UploadedAttachment { name: string; path: string; size: number }

/** Keep the existing chunked, atomic, workspace-confined upload protocol. */
export async function uploadFile(conn: Connection, sessionId: string, file: File, path: string, options: {
  overwrite?: boolean
  signal?: AbortSignal
  progress?: (percent: number) => void
} = {}): Promise<void> {
  if (file.size > MAX_UPLOAD_SIZE) throw new Error('单个文件上传上限为 100 MB')
  const uploadId = crypto.randomUUID()
  const chunks = new TransferChunkSizer()
  try {
    let offset = 0
    do {
      options.signal?.throwIfAborted()
      if (!conn.isOpen) throw new Error('连接已断开，请重试上传')
      const bytes = new Uint8Array(await file.slice(offset, offset + chunks.size).arrayBuffer())
      options.signal?.throwIfAborted()
      let binary = ''
      for (const byte of bytes) binary += String.fromCharCode(byte)
      const next = offset + bytes.length
      const final = next === file.size
      const started = performance.now()
      const env = await conn.request<{ path: string; size?: number }>(MessageType.FileWrite, {
        session_id: sessionId, upload_id: uploadId, path, size: file.size,
        offset, data: btoa(binary), final, overwrite: options.overwrite ?? false,
      }, sessionId)
      if (env.type !== (final ? MessageType.FileWriteDone : MessageType.FileWriteReady)
        || env.payload?.path !== path || (final && env.payload.size !== file.size)) {
        throw new Error('上传确认不匹配，请刷新工作区文件后重试')
      }
      chunks.observe(performance.now() - started)
      offset = next
      options.progress?.(file.size === 0 ? 100 : Math.round(offset / file.size * 100))
      if (final) return
    } while (offset < file.size)
  } catch (error) {
    // Cancel only this upload's temporary file; completed destinations are kept.
    if (conn.isOpen) {
      await conn.request(MessageType.FileCancel, { session_id: sessionId, transfer_id: uploadId }, sessionId).catch(() => {})
    }
    throw error
  }
}

/** A unique, portable basename: no overwrite, traversal or control characters. */
export function attachmentFilename(name: string): string {
  const safe = name.normalize('NFKC').replace(/[^a-zA-Z0-9._-]/g, '_').replace(/^\.+/, '') || 'file'
  const dot = safe.lastIndexOf('.')
  const extension = dot > 0 ? safe.slice(dot).slice(0, 16) : ''
  const stem = dot > 0 ? safe.slice(0, dot) : safe
  return `codegate-${crypto.randomUUID()}-${stem.slice(0, 48)}${extension}`
}

export function workspaceFilePath(cwd: string, filename: string): string {
  const separator = cwd.includes('\\') ? '\\' : '/'
  return cwd.replace(/[\\/]+$/, '') + separator + filename
}
