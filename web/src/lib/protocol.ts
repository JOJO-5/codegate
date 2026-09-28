/**
 * CodeGate 协议层 —— Go 侧 `internal/protocol` 的 TypeScript 镜像。
 *
 * 这个文件是**纯逻辑**：不碰网络、不碰 DOM、不碰 xterm。它只负责三件事：
 *
 *   1. 声明控制消息的信封与各 payload 结构（字段名与 Go 的 json tag 逐字对齐）
 *   2. 20 字节二进制帧的编解码
 *   3. UUID 字符串 ↔ 16 字节的互转（帧头的 StreamID 是裸字节，不是字符串）
 *
 * # 为什么手写而不从 Go 生成
 *
 * 协议面很小（二十来个类型），而代码生成器会带来构建期依赖、版本漂移，
 * 以及一堆没人看得懂的中间产物。手写的代价是必须靠人保证两边同步 ——
 * 所以下面每个类型都标了对应的 Go 结构名，改一边时另一边有据可查。
 *
 * # 一条必须守住的纪律
 *
 * 这里的类型只描述**浏览器会用到**的字段。Agent 专属的消息
 * （agent.hello / agent.auth / session.sync …）也一并声明，是因为
 * 前端需要在收到它们时能识别出「这不该出现」——协议层故意不实现
 * 「静默忽略未知类型」，见 `MessageType` 的注释。
 */

/** 协议版本。只在破坏性改动时递增（§9.4）。 */
export const PROTOCOL_VERSION = 1

/** 二进制帧固定头长度。`internal/protocol/binary.go: BinaryHeaderLen`。 */
export const BINARY_HEADER_LEN = 20

// ---------------------------------------------------------------------------
// 二进制帧
// ---------------------------------------------------------------------------

/**
 * 帧类型。`internal/protocol/binary.go: FrameType`。
 *
 * ★ 这里用 `as const` 对象而不是 TS `enum`：
 *   - enum 会生成运行时代码，且 `const enum` 与 `isolatedModules` 冲突
 *   - 对象字面量可以直接 `Object.values()` 遍历做校验
 */
export const FrameType = {
  /** 浏览器 → PTY 的输入字节。 */
  Stdin: 0x01,
  /** PTY → 浏览器的实时输出。 */
  Stdout: 0x02,
  /** PTY → 浏览器的历史重放（attach 时）。 */
  Buffer: 0x03,
  /** 文件传输字节块，双向。 */
  FileData: 0x10,
} as const
export type FrameType = (typeof FrameType)[keyof typeof FrameType]

/** 帧标志位。`internal/protocol/binary.go: Flag*`。 */
export const FrameFlag = {
  /** 重放结束，后续就是实时流了。 */
  BufferEnd: 1 << 0,
  /** 该会话有字节被丢弃，客户端应做一次全量重放。 */
  Dropped: 1 << 1,
  /** 本帧是该传输的最后一块（文件传输用）。 */
  Final: 1 << 2,
} as const

/** 全部已知标志位。用于校验「未知位必须拒绝」这条纪律。 */
const KNOWN_FLAGS = FrameFlag.BufferEnd | FrameFlag.Dropped | FrameFlag.Final

/**
 * 判断一个字节是否是我们认识的帧类型。
 *
 * 前端收到未知帧类型时的正确处理是**断开连接**而不是忽略 ——
 * 忽略会掩盖「协议版本不匹配」，症状会变成难查的输出乱码。
 */
export function isKnownFrameType(t: number): t is FrameType {
  return t === FrameType.Stdin || t === FrameType.Stdout || t === FrameType.Buffer || t === FrameType.FileData
}

/** 解析后的二进制帧。 */
export interface DecodedFrame {
  type: FrameType
  flags: number
  /** 终端帧里是 SessionID，文件帧里是 TransferID。统一按小写 UUID 字符串给出。 */
  streamId: string
  /**
   * payload 是入参 buffer 的**视图**，没有复制。
   * 调用方（xterm.write）会同步消费它，所以不必复制 ——
   * 每帧复制一次在终端高频输出下是纯浪费。
   */
  payload: Uint8Array
}

// ---------------------------------------------------------------------------
// 控制消息类型
// ---------------------------------------------------------------------------

/**
 * 消息类型。`internal/protocol/types.go` 的常量表。
 *
 * ★ 这里把 Agent 专属类型也列出来，是有意的：前端一旦收到
 *   `session.exit` / `agent.ready` 这类消息，说明要么是服务端有 bug，
 *   要么是有人在伪造。两种都值得报错，不值得静默吞掉。
 */
export const MessageType = {
  // ---- Agent 连接与鉴权（浏览器永远不该收到）----
  AgentHello: 'agent.hello',
  AgentChallenge: 'agent.challenge',
  AgentAuth: 'agent.auth',
  AgentReady: 'agent.ready',
  AgentHeartbeat: 'agent.heartbeat',

  // ---- 设备配对 ----
  AgentPairBegin: 'agent.pair.begin',
  AgentPairCode: 'agent.pair.code',
  AgentPairCompleted: 'agent.pair.completed',

  // ---- 设备信息 ----
  DeviceInfo: 'device.info',

  // ---- 会话 ----
  SessionSync: 'session.sync',
  SessionCreate: 'session.create',
  SessionCreated: 'session.created',
  SessionList: 'session.list',
  SessionListed: 'session.list.result',
  SessionGet: 'session.get',
  SessionInfo: 'session.info',
  SessionAttach: 'session.attach',
  SessionAttached: 'session.attached',
  SessionDetach: 'session.detach',
  SessionDetached: 'session.detached',
  SessionClose: 'session.close',
  SessionClosed: 'session.closed',
  SessionResize: 'session.resize',
  SessionSignal: 'session.signal',
  SessionExit: 'session.exit',

  // ---- 多客户端（§7.6）----
  SessionClaimControl: 'session.claim_control',
  SessionRoleChanged: 'session.role_changed',

  // ---- 文件传输 ----
  FileList: 'file.list',
  FileListed: 'file.list.result',
  FileStat: 'file.stat',
  FileStatResult: 'file.stat.result',
  FileRead: 'file.read',
  FileReadBegin: 'file.read.begin',
  FileAck: 'file.ack',
  FileWrite: 'file.write',
  FileWriteReady: 'file.write.ready',
  FileWriteDone: 'file.write.done',
  FileCancel: 'file.cancel',

  // ---- 通用 ----
  Error: 'error',
  Ping: 'ping',
  Pong: 'pong',
} as const
export type MessageType = (typeof MessageType)[keyof typeof MessageType]

/** 控制消息信封（§9.2）。`internal/protocol/message.go: Envelope`。 */
export interface Envelope<P = unknown> {
  /** 协议版本，必填。 */
  v: number
  /** 消息类型，必填。 */
  type: MessageType
  /** 请求方生成，响应方在 replyTo 里回填。 */
  requestId?: string
  /** 回填被响应请求的 requestId。 */
  replyTo?: string
  /** 会话相关消息建议都带，便于日志与路由。 */
  sessionId?: string
  /** 毫秒时间戳。**仅作参考，不得用于任何逻辑判断**（两端时钟不同步）。 */
  ts?: number
  payload?: P
}

/**
 * 把线上信封（snake_case）转成前端内部形状（camelCase）。
 *
 * 保留这个转换层而不是让前端到处写 `env.request_id`：内部统一用 camelCase
 * 能过 lint，也避免有人误把 `request_id` 写成 `requestId` 去查一个不存在的字段
 * —— 那种 bug 在 TS 里不会报错（索引签名），只会在运行时静默变成 undefined。
 */
export function parseEnvelope(raw: unknown): Envelope {
  if (typeof raw !== 'object' || raw === null) {
    throw new ProtocolError('信封不是对象')
  }
  const o = raw as Record<string, unknown>
  const type = o['type']
  if (typeof type !== 'string') {
    throw new ProtocolError('信封缺少 type')
  }
  return {
    v: typeof o['v'] === 'number' ? o['v'] : 0,
    type: type as MessageType,
    requestId: typeof o['request_id'] === 'string' ? o['request_id'] : undefined,
    replyTo: typeof o['reply_to'] === 'string' ? o['reply_to'] : undefined,
    sessionId: typeof o['session_id'] === 'string' ? o['session_id'] : undefined,
    ts: typeof o['ts'] === 'number' ? o['ts'] : undefined,
    payload: o['payload'],
  }
}

/** 把前端内部形状序列化成线上信封（去掉 undefined，字段名回 snake_case）。 */
export function serializeEnvelope(env: Envelope): Record<string, unknown> {
  const out: Record<string, unknown> = { v: env.v, type: env.type }
  if (env.requestId !== undefined) out['request_id'] = env.requestId
  if (env.replyTo !== undefined) out['reply_to'] = env.replyTo
  if (env.sessionId !== undefined) out['session_id'] = env.sessionId
  if (env.ts !== undefined) out['ts'] = env.ts
  out['payload'] = env.payload ?? {}
  return out
}

/** 协议层的本地错误（不是服务端返回的业务错误）。 */
export class ProtocolError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ProtocolError'
  }
}

// ---------------------------------------------------------------------------
// 错误码（§9.3 + 实现补充）
// ---------------------------------------------------------------------------

/**
 * 错误码枚举。`internal/protocol/errors.go`。
 *
 * `not_found` 是实现阶段补的一个码：为了让「不存在」和「无权访问」
 * 返回**同一个**状态码、错误码与文案，从而消除跨端点的存在性预言机。
 * 所以前端拿到 not_found 时，含义是「目标不存在**或**你无权访问」，
 * 文案必须照此写，不能写成「设备不存在」。
 */
export const ErrorCode = {
  InvalidMessage: 'invalid_message',
  InvalidPayload: 'invalid_payload',
  Unauthenticated: 'unauthenticated',
  Forbidden: 'forbidden',
  NotFound: 'not_found',
  DeviceOffline: 'device_offline',
  DeviceNotPaired: 'device_not_paired',
  SessionNotFound: 'session_not_found',
  SessionLimitReached: 'session_limit_reached',
  CwdNotAllowed: 'cwd_not_allowed',
  CommandNotAllowed: 'command_not_allowed',
  SessionAlreadyAttached: 'session_already_attached',
  FrameTooLarge: 'frame_too_large',
  RateLimited: 'rate_limited',
  Internal: 'internal',
} as const
export type ErrorCode = (typeof ErrorCode)[keyof typeof ErrorCode]

/** `error` 消息的 payload。`protocol.ErrorPayload`。 */
export interface ErrorPayload {
  code: ErrorCode | string
  message: string
  retryable: boolean
}

// ---------------------------------------------------------------------------
// 会话相关 payload
// ---------------------------------------------------------------------------

/**
 * 会话对外快照。`protocol.SessionSummary`。
 *
 * 这个结构同时出现在 Agent 上报、Server 转发、REST 响应三处 ——
 * 三处用同一个类型，前端就只需要一套解析逻辑。
 */
export interface SessionSummary {
  session_id: string
  device_id: string
  name: string
  command: string
  args?: string[]
  cwd: string
  /** starting | running | detached | exited | failed | terminated */
  status: SessionStatus
  pid?: number
  exit_code?: number | null
  cols: number
  rows: number
  created_at: number
  started_at?: number | null
  ended_at?: number | null
  last_attached_at?: number | null
  /** ring buffer 里现存输出的序号区间，用于判断断线期间丢了多少。 */
  buffer_seq_from: number
  buffer_seq_to: number
}

export type SessionStatus = 'starting' | 'running' | 'detached' | 'exited' | 'failed' | 'terminated'

/** `protocol.SessionCreatePayload`。 */
export interface SessionCreatePayload {
  device_id: string
  name?: string
  /** 与 command/args 二选一：走白名单时只传这个。 */
  command_id?: string
  command?: string
  args?: string[]
  cwd: string
  cols: number
  rows: number
  /** 恢复上次对话（§21.3）。 */
  resume?: boolean
}

export interface SessionCreatedPayload {
  session: SessionSummary
}

export interface SessionListedPayload {
  sessions: SessionSummary[]
}

/** `protocol.SessionAttachPayload`。 */
export interface SessionAttachPayload {
  session_id: string
  /**
   * 0 = 我没有历史，请把 ring buffer 里能给的都给。
   * lastSeq = 我已有到 lastSeq 的输出，只要它之后的。
   */
  since: number
  cols: number
  rows: number
}

/** `protocol.SessionAttachedPayload`。 */
export interface SessionAttachedPayload {
  session: SessionSummary
  /**
   * 本次重放覆盖的区间。
   * ★ 若 seq_from > 请求的 since，说明我落后太多、中间有丢帧 ——
   *   必须**清屏**后按 seq_from 重放，否则屏幕上会拼出错误的画面。
   */
  seq_from: number
  seq_to: number
  /** controller | viewer（§7.6 多客户端）。 */
  role: 'controller' | 'viewer'
}

export interface SessionDetachedPayload {
  session_id: string
  /** client_close | superseded | idle | server_shutdown */
  reason: string
}

export interface SessionClosedPayload {
  session_id: string
  reason: string
}

export interface SessionExitPayload {
  session_id: string
  exit_code: number
  /** exited | signal | pty_error */
  reason?: string
}

export interface SessionRoleChangedPayload {
  session_id: string
  controller: string
}

export interface SessionResizePayload {
  session_id: string
  cols: number
  rows: number
}

export interface SessionSignalPayload {
  session_id: string
  /** int | term | kill。★ Windows 上只有 int 有真实实现，且对 cooked mode 无效，见 §6.5。 */
  signal: string
}

export interface SessionDetachRequestPayload {
  session_id: string
  reason?: string
}

export interface SessionCloseRequestPayload {
  session_id: string
  force?: boolean
}

// ---------------------------------------------------------------------------
// UUID ↔ 字节
// ---------------------------------------------------------------------------

/**
 * 把 UUID 字符串转成 16 字节。
 *
 * 帧头的 StreamID 是裸的 16 字节（`binary.go` 里 `copy(hdr[4:20], sid[:])`），
 * 不是字符串，所以每次发 stdin 都要做这个转换。
 */
export function uuidToBytes(uuid: string): Uint8Array {
  const hex = uuid.replace(/-/g, '')
  if (hex.length !== 32 || !/^[0-9a-fA-F]{32}$/.test(hex)) {
    throw new ProtocolError(`不是合法 UUID: ${uuid}`)
  }
  const out = new Uint8Array(16)
  for (let i = 0; i < 16; i++) {
    out[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16)
  }
  return out
}

/** 把 16 字节转回带连字符的小写 UUID 字符串。 */
export function bytesToUuid(bytes: Uint8Array): string {
  if (bytes.length < 16) {
    throw new ProtocolError(`UUID 字节不足 16：${bytes.length}`)
  }
  let hex = ''
  for (let i = 0; i < 16; i++) {
    hex += bytes[i].toString(16).padStart(2, '0')
  }
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

// ---------------------------------------------------------------------------
// 帧编解码
// ---------------------------------------------------------------------------

/**
 * 编码一帧。
 *
 *   ┌─────────┬────────┬────────┬──────────────┬───────────────┐
 *   │ Version │  Type  │ Flags  │   StreamID   │    Payload    │
 *   │  1 byte │ 1 byte │ 2 byte │   16 bytes   │    N bytes    │
 *   └─────────┴────────┴────────┴──────────────┴───────────────┘
 *
 * ★ 头里**没有** payload 长度字段：WebSocket 帧本身携带长度。
 *   再加一个只会引入「两个长度不一致怎么办」的解析攻击面。
 */
export function encodeFrame(
  type: FrameType,
  flags: number,
  streamId: string,
  payload: Uint8Array,
): Uint8Array {
  if ((flags & ~KNOWN_FLAGS) !== 0) {
    throw new ProtocolError(`非法标志位: 0x${flags.toString(16)}`)
  }
  const out = new Uint8Array(BINARY_HEADER_LEN + payload.length)
  const view = new DataView(out.buffer)
  out[0] = 0x01
  out[1] = type
  view.setUint16(2, flags, false) // big-endian，与 Go 的 binary.BigEndian 对齐
  out.set(uuidToBytes(streamId), 4)
  out.set(payload, BINARY_HEADER_LEN)
  return out
}

/**
 * 解析一帧。校验顺序从廉价到昂贵，且**不做任何容错**。
 *
 * 这里宽容一次，后面就要到处打补丁：一个被忽略的坏帧会变成
 * 「屏幕上有几行乱码」这种极难定位的症状，而直接抛错能立刻定位到连接层。
 */
export function decodeFrame(buf: ArrayBuffer): DecodedFrame {
  if (buf.byteLength < BINARY_HEADER_LEN) {
    throw new ProtocolError(`帧太短：${buf.byteLength} 字节，需要至少 ${BINARY_HEADER_LEN}`)
  }
  const all = new Uint8Array(buf)
  if (all[0] !== 0x01) {
    throw new ProtocolError(`帧版本不支持: 0x${all[0].toString(16)}`)
  }
  const type = all[1]
  if (!isKnownFrameType(type)) {
    throw new ProtocolError(`未知帧类型: 0x${type.toString(16)}`)
  }
  const flags = new DataView(buf).getUint16(2, false)
  if ((flags & ~KNOWN_FLAGS) !== 0) {
    throw new ProtocolError(`非法标志位: 0x${flags.toString(16)}`)
  }
  return {
    type,
    flags,
    streamId: bytesToUuid(all.subarray(4, 20)),
    payload: all.subarray(BINARY_HEADER_LEN),
  }
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

/** 判断状态是否为「进程已经结束」（三种终态）。 */
export function isTerminalStatus(s: SessionStatus): boolean {
  return s === 'exited' || s === 'failed' || s === 'terminated'
}

/** 判断会话此刻是否活着（可以 attach）。 */
export function isLiveStatus(s: SessionStatus): boolean {
  return s === 'starting' || s === 'running' || s === 'detached'
}
