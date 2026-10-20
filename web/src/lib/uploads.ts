import type { useConnStore } from '../stores/conn'
import { MessageType } from './protocol'

export const MAX_UPLOAD_SIZE = 100 * 1024 * 1024
type Connection = Pick<ReturnType<typeof useConnStore>, 'isOpen' | 'request'>
export interface UploadedAttachment { name: string; path: string; size: number }

// Keep each non-preemptible message small, but overlap its acknowledgement.
// RTT is not a bandwidth measurement: shrinking blocks on a distant connection
// made stop-and-wait uploads slower still. Bound outstanding data to 128 KiB.
const UPLOAD_CHUNK_SIZE = 32 * 1024
const UPLOAD_WINDOW = 4

/** Ordered bounded pipelining over the existing atomic workspace upload protocol. */
export async function uploadFile(conn: Connection, sessionId: string, file: File, path: string, options: {
  overwrite?: boolean
  signal?: AbortSignal
  progress?: (percent: number) => void
} = {}): Promise<void> {
  if (file.size > MAX_UPLOAD_SIZE) throw new Error('单个文件上传上限为 100 MB')
  const uploadId = crypto.randomUUID()
  type Receipt = { next: number; error?: never } | { error: unknown; next?: never }
  const pending: Promise<Receipt>[] = []
  let failed = false
  let failure: unknown
  let failNow!: (error: unknown) => void
  const rejected = new Promise<never>((_, reject) => { failNow = reject })
  void rejected.catch(() => {})
  let onAbort: (() => void) | undefined
  const aborted = new Promise<never>((_, reject) => {
    onAbort = () => reject(new DOMException('上传已取消', 'AbortError'))
  })
  // The signal can fire while preparing a block, before the first race awaits it.
  void aborted.catch(() => {})
  options.signal?.addEventListener('abort', onAbort!, { once: true })
  const check = (): void => {
    options.signal?.throwIfAborted()
    if (failed) throw failure
    if (!conn.isOpen) throw new Error('连接已断开，请重试上传')
  }
  const acknowledge = async (): Promise<void> => {
    const receipt = await Promise.race([pending.shift()!, aborted, rejected])
    check()
    if ('error' in receipt) throw receipt.error
    // A rounded percentage must not claim publication before the final receipt.
    options.progress?.(file.size === 0 || receipt.next === file.size ? 100 : Math.min(99, Math.floor(receipt.next / file.size * 100)))
  }
  try {
    let offset = 0
    do {
      check()
      const bytes = new Uint8Array(await file.slice(offset, offset + UPLOAD_CHUNK_SIZE).arrayBuffer())
      const next = offset + bytes.length
      const final = next === file.size
      // Never queue the publishing block behind unconfirmed data. Errors or
      // cancellation must stop the upload before a destination can appear.
      if (final) while (pending.length) await acknowledge()
      else if (pending.length >= UPLOAD_WINDOW) await acknowledge()
      check()
      const receipt = conn.request<{ path: string; size?: number }>(MessageType.FileWrite, {
        session_id: sessionId, upload_id: uploadId, path, size: file.size,
        offset, data: btoa(String.fromCharCode(...bytes)), final, overwrite: options.overwrite ?? false,
      }, sessionId).then(env => {
        if (env.type !== (final ? MessageType.FileWriteDone : MessageType.FileWriteReady)
          || env.payload?.path !== path || (final && env.payload.size !== file.size)) {
          throw new Error('上传确认不匹配，请刷新工作区文件后重试')
        }
        return { next }
      }).catch(error => {
        // Handle every outstanding rejection, even after cancellation. Record
        // early failures immediately so a late earlier receipt cannot resume send.
        failed = true; failure = error; failNow(error)
        return { error }
      })
      pending.push(receipt)
      offset = next
      if (final) { await acknowledge(); return }
    } while (offset < file.size)
  } catch (error) {
    // Cancel only this upload's temporary file; completed destinations are kept.
    if (conn.isOpen) {
      await conn.request(MessageType.FileCancel, { session_id: sessionId, transfer_id: uploadId }, sessionId).catch(() => {})
    }
    throw error
  } finally { options.signal?.removeEventListener('abort', onAbort!) }
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
