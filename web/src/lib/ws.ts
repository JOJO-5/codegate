/**
 * 浏览器侧的 WebSocket 客户端。
 *
 * 它负责四件事，每一件都有明确的失败模式：
 *
 *   1. **取票 + 建连**：`POST /ws-ticket` 拿一次性票据（TTL 30s），
 *      再拿它去换 WS 连接。票据必须每次重连都重新取 —— 它是**用完即焚**的。
 *
 *   2. **请求-响应匹配**：控制消息是异步的，靠 `request_id` / `reply_to`
 *      配对。没有这一层，每个调用点都要自己挂监听器、自己超时、自己清理，
 *      迟早会漏掉清理导致内存泄漏。
 *
 *   3. **二进制帧收发**：终端字节流走 Binary Frame，**不做任何 UTF-8 解码**。
 *      解码是 xterm.js 的职责 —— 它做的是增量解码，能正确处理跨帧的多字节字符。
 *      在这里先解一次会把跨帧的字符撕成两半，表现为屏幕上出现乱码方块。
 *
 *   4. **重连 + 恢复**：指数退避 1s → 2s → … → 30s（带 jitter）。
 *      重连成功后必须让上层有机会用 `since: lastSeq` 重新 attach，
 *      这就是「断网 10 秒后无感恢复」的全部实现（§8.2）。
 *
 * # 为什么不在这里自动 attach
 *
 * 因为「重新 attach 谁」是上层的知识。这个类只知道连接，不知道页面。
 * 它把「重连成功」作为事件抛出去（`onReady(reconnect=true)`），
 * 由终端页决定要不要恢复。这个边界一旦模糊，WS 层就会开始长业务逻辑。
 */

import {
  FrameType,
  MessageType,
  PROTOCOL_VERSION,
  ProtocolError,
  decodeFrame,
  encodeFrame,
  parseEnvelope,
  serializeEnvelope,
  type DecodedFrame,
  type Envelope,
  type ErrorPayload,
} from './protocol'
import { ulid } from './ulid'
import { api } from './api'

/** 连接状态。 */
export type WSState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface WSClientOptions {
  /** 收到一条**非响应**的控制消息（事件、通知、error）。 */
  onControl?: (env: Envelope) => void
  /** 收到一个二进制帧。★ 调用方必须同步消费 payload，它不保证在下一个 tick 后仍有效。 */
  onFrame?: (frame: DecodedFrame) => void
  /** 状态变化，用于 UI 显示。 */
  onState?: (state: WSState, detail?: string) => void
  /** 连接就绪。`reconnect` 为 true 时上层应当做 attach 恢复。 */
  onReady?: (reconnect: boolean) => void
  /** 不可恢复的失败（需要重新登录 / 协议不兼容）。**不会再重连。** */
  onFatal?: (reason: string) => void
}

/** 单个请求的超时。服务端的 pending 表 TTL 是 60s，这里取一半。 */
const REQUEST_TIMEOUT_MS = 30_000

/** 退避参数。 */
const BACKOFF_BASE_MS = 1_000
const BACKOFF_MAX_MS = 30_000

/**
 * 这些关闭码表示「重连也没用」，必须停下来。
 *
 *   4401 认证失败     —— 票据/会话已经无效，重连只会一直 4401
 *   4426 版本不兼容   —— 重连一百次版本也不会变
 *
 * 其余码（1001 服务端要关、1013 过载、1006 异常断开…）都值得重试 ——
 * 尤其是 1013：服务端过载时立刻重连等于帮着打自己，退避正好是解药。
 */
const FATAL_CLOSE_CODES = new Set([4401, 4426])

interface PendingRequest {
  resolve: (env: Envelope) => void
  reject: (err: Error) => void
  timer: ReturnType<typeof setTimeout>
}

export class WSClient {
  private ws: WebSocket | null = null
  private state: WSState = 'idle'
  private readonly opts: WSClientOptions

  /** 请求登记表：request_id → 待决 Promise。 */
  private readonly pending = new Map<string, PendingRequest>()

  /** 已建立的连接次数。>0 说明这次是重连。 */
  private connects = 0

  /** 重连尝试次数，用于算退避。成功连接后清零。 */
  private attempt = 0

  /** 是否被主动关闭。主动关闭不再重连。 */
  private disposed = false

  private retryTimer: ReturnType<typeof setTimeout> | null = null

  constructor(opts: WSClientOptions = {}) {
    this.opts = opts
  }

  getState(): WSState {
    return this.state
  }

  isOpen(): boolean {
    return this.ws !== null && this.ws.readyState === WebSocket.OPEN
  }

  // -------------------------------------------------------------------------
  // 连接管理
  // -------------------------------------------------------------------------

  /** 建立连接。重复调用是安全的（已在连或已连上时直接返回）。 */
  connect(): void {
    if (this.disposed) return
    if (this.state === 'connecting' || this.state === 'open') return
    void this.openOnce()
  }

  /** 主动断开，不再重连。 */
  dispose(): void {
    this.disposed = true
    this.clearRetry()
    this.rejectAllPending(new Error('连接已关闭'))
    if (this.ws !== null) {
      try {
        this.ws.close(1000, 'client_dispose')
      } catch {
        // 已经断了，忽略
      }
      this.ws = null
    }
    this.setState('closed')
  }

  private async openOnce(): Promise<void> {
    this.setState(this.connects === 0 ? 'connecting' : 'reconnecting')

    // ---- 1. 取票 ----
    let ticket: string
    let proto: { min: number; max: number }
    try {
      const t = await api.wsTicket()
      ticket = t.ticket
      proto = t.protocol
    } catch (e) {
      // 取票失败通常是 401（会话失效）或网络问题。
      // 401 由 api 层已经触发 authLost，这里只需要停止重连。
      this.setState('closed', e instanceof Error ? e.message : '取票失败')
      this.opts.onFatal?.(e instanceof Error ? e.message : '取票失败')
      return
    }

    // ---- 2. 版本协商（§9.4）----
    //
    // 提前拒绝不兼容的页面，比连上去之后收到一堆看不懂的帧要好得多 ——
    // 后者的症状是「终端显示乱码」，而真正的原因在协议版本上。
    if (PROTOCOL_VERSION < proto.min || PROTOCOL_VERSION > proto.max) {
      const msg = `前端协议版本 ${PROTOCOL_VERSION} 不在服务端支持区间 [${proto.min}, ${proto.max}] 内，请刷新页面或更新部署`
      this.setState('closed', msg)
      this.opts.onFatal?.(msg)
      return
    }

    if (this.disposed) return

    // ---- 3. 建连 ----
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${scheme}//${location.host}/api/v1/ws/client?ticket=${encodeURIComponent(ticket)}`

    let ws: WebSocket
    try {
      ws = new WebSocket(url)
    } catch (e) {
      this.scheduleRetry(e instanceof Error ? e.message : '建连失败')
      return
    }
    ws.binaryType = 'arraybuffer' // 不设的话浏览器会给 Blob，而 Blob 是异步读的
    this.ws = ws

    ws.onopen = () => {
      this.connects++
      this.attempt = 0
      this.setState('open')
      this.opts.onReady?.(this.connects > 1)
    }

    ws.onmessage = (ev: MessageEvent) => {
      this.handleMessage(ev)
    }

    ws.onerror = () => {
      // onerror 之后一定会有 onclose，所以这里不做事。
      // 在这里重连会导致同一次失败触发两条重连路径。
    }

    ws.onclose = (ev: CloseEvent) => {
      this.ws = null
      this.rejectAllPending(new Error(`连接已断开 (${ev.code})`))

      if (this.disposed) {
        this.setState('closed')
        return
      }
      if (FATAL_CLOSE_CODES.has(ev.code)) {
        const msg =
          ev.code === 4401 ? '登录已失效，请重新登录' : '前端与服务端协议版本不兼容'
        this.setState('closed', msg)
        this.opts.onFatal?.(msg)
        return
      }
      this.scheduleRetry(`连接关闭 (${ev.code} ${ev.reason || '无原因'})`)
    }
  }

  /** 安排一次重连。 */
  private scheduleRetry(detail: string): void {
    if (this.disposed || this.retryTimer !== null) return

    this.setState('reconnecting', detail)

    // 指数退避 + jitter。
    //
    // jitter 不是可有可无的：服务端重启时所有客户端会在同一毫秒断开，
    // 没有 jitter 的话它们会同步重连，形成周期性冲击波（thundering herd）。
    const exp = Math.min(BACKOFF_BASE_MS * 2 ** this.attempt, BACKOFF_MAX_MS)
    const delay = Math.round(exp * (0.75 + Math.random() * 0.5))
    this.attempt++

    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      void this.openOnce()
    }, delay)
  }

  private clearRetry(): void {
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
  }

  private setState(s: WSState, detail?: string): void {
    this.state = s
    this.opts.onState?.(s, detail)
  }

  // -------------------------------------------------------------------------
  // 收消息
  // -------------------------------------------------------------------------

  private handleMessage(ev: MessageEvent): void {
    // ---- 二进制帧 ----
    if (ev.data instanceof ArrayBuffer) {
      let frame: DecodedFrame
      try {
        frame = decodeFrame(ev.data)
      } catch (e) {
        // 坏帧不忽略 —— 忽略会变成「屏幕上有几行乱码」这种极难定位的症状。
        // 直接断连，让重连 + 全量重放把状态拉回正轨。
        const msg = e instanceof ProtocolError ? e.message : '非法二进制帧'
        this.setState('reconnecting', msg)
        this.ws?.close(1008, 'invalid_frame')
        return
      }
      this.opts.onFrame?.(frame)
      return
    }

    // ---- 控制消息（Text）----
    if (typeof ev.data !== 'string') {
      return
    }

    let env: Envelope
    try {
      env = parseEnvelope(JSON.parse(ev.data))
    } catch {
      this.ws?.close(1008, 'invalid_message')
      return
    }

    // 响应：按 reply_to 归还给等待中的 Promise
    if (env.replyTo !== undefined && env.replyTo !== '') {
      const p = this.pending.get(env.replyTo)
      if (p !== undefined) {
        this.pending.delete(env.replyTo)
        clearTimeout(p.timer)
        if (env.type === MessageType.Error) {
          const ep = env.payload as ErrorPayload | undefined
          p.reject(new WSError(ep?.code ?? 'internal', ep?.message ?? '请求失败', ep?.retryable ?? false))
        } else {
          p.resolve(env)
        }
        return
      }
      // reply_to 指向一个不存在的请求：说明服务端超时清理后响应才到。
      // 这是正常竞态，降级成普通事件交给上层，不报错。
    }

    this.opts.onControl?.(env)
  }

  // -------------------------------------------------------------------------
  // 发消息
  // -------------------------------------------------------------------------

  /**
   * 发一个请求并等它的响应。
   *
   * @param type      消息类型（必须是浏览器有权发出的）
   * @param payload   payload 对象
   * @param sessionId 会话相关消息应当带上
   */
  request<T = unknown>(type: MessageType, payload: unknown, sessionId?: string): Promise<Envelope<T>> {
    return new Promise<Envelope<T>>((resolve, reject) => {
      if (!this.isOpen()) {
        reject(new WSError('unavailable', '连接未就绪，请稍候重试', true))
        return
      }
      const id = ulid()
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new WSError('timeout', '请求超时，请重试', true))
      }, REQUEST_TIMEOUT_MS)

      this.pending.set(id, {
        resolve: resolve as (env: Envelope) => void,
        reject,
        timer,
      })

      try {
        this.sendEnvelope({
          v: PROTOCOL_VERSION,
          type,
          requestId: id,
          ...(sessionId !== undefined ? { sessionId } : {}),
          ts: Date.now(),
          payload,
        })
      } catch (e) {
        this.pending.delete(id)
        clearTimeout(timer)
        reject(e instanceof Error ? e : new Error(String(e)))
      }
    })
  }

  /** 发一条不需要响应的消息（detach / resize / signal）。 */
  notify(type: MessageType, payload: unknown, sessionId?: string): void {
    if (!this.isOpen()) return
    try {
      this.sendEnvelope({
        v: PROTOCOL_VERSION,
        type,
        ...(sessionId !== undefined ? { sessionId } : {}),
        ts: Date.now(),
        payload,
      })
    } catch {
      // 通知类消息发不出去不算错误：连接断了的话重连后上层会重新同步状态。
    }
  }

  /**
   * 发一个二进制帧。
   *
   * ★ payload 必须是**原始字节**，不能是字符串。终端输入走 TextEncoder，
   *   输出走 xterm.write(Uint8Array)，两端都不做编解码。
   */
  sendFrame(type: FrameType, flags: number, streamId: string, payload: Uint8Array): void {
    if (!this.isOpen()) return
    this.ws?.send(encodeFrame(type, flags, streamId, payload))
  }

  private sendEnvelope(env: Envelope): void {
    const ws = this.ws
    if (ws === null || ws.readyState !== WebSocket.OPEN) {
      throw new WSError('unavailable', '连接未就绪', true)
    }
    ws.send(JSON.stringify(serializeEnvelope(env)))
  }

  private rejectAllPending(err: Error): void {
    for (const [, p] of this.pending) {
      clearTimeout(p.timer)
      p.reject(err)
    }
    this.pending.clear()
  }
}

/** WS 层的业务错误（由服务端 `error` 消息翻译而来）。 */
export class WSError extends Error {
  readonly code: string
  readonly retryable: boolean

  constructor(code: string, message: string, retryable: boolean) {
    super(message)
    this.name = 'WSError'
    this.code = code
    this.retryable = retryable
  }
}
