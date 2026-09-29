/**
 * REST 客户端。
 *
 * 三个必须做对的地方，每一个都有对应的服务端设计：
 *
 * # 1. refresh token 只走 HttpOnly Cookie，前端碰不到
 *
 * 服务端的 `tokenResponse` **刻意不含 refresh_token 字段**
 * （见 `internal/server/handler_auth.go` 的注释：一旦它出现在 JSON 里，
 * 前端一定会有人把它存进 localStorage —— 那等于把「Cookie 防 XSS」
 * 这条设计整个作废）。
 *
 * 所以这里所有请求都必须带 `credentials: 'include'`，
 * 并且**不提供任何读取 refresh token 的接口** —— 它是 HttpOnly 的，读不到，
 * 假装能读只会让人写出永远为 null 的代码。
 *
 * # 2. 401 只能重试一次
 *
 * 否则一个真正失效的会话会变成无限循环的刷新风暴。
 *
 * # 3. 刷新必须去重（这条最容易写错，后果最严重）
 *
 * 终端页会同时发多个请求（设备列表 + 会话列表 + WS 票据）。如果每个 401
 * 都各自去 refresh，就会并发刷新同一个令牌 —— 而服务端的 refresh token 是
 * **轮换 + 重用检测**的：并发使用同一个 refresh token 会被判定为重放攻击，
 * 触发**整族吊销**，用户被直接踢下线，而且看起来像是"随机掉线"。
 *
 * 所以这里用一个共享的 in-flight Promise，把并发刷新收敛成一次。
 */

import type { SessionSummary } from './protocol'

const BASE = '/api/v1'

/**
 * 这些路径上的 401 不触发自动刷新。
 *
 * login/register 的 401 是「密码错了」，刷新救不了；
 * refresh 自己的 401 是「refresh token 也没了」，再去刷新就是死循环。
 */
const NO_AUTO_REFRESH = new Set(['/auth/login', '/auth/register', '/auth/refresh'])

/** 服务端返回的业务错误。 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryable: boolean

  constructor(status: number, code: string, message: string, retryable = false) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.retryable = retryable
  }

  /** 凭证失效（需要重新登录）。 */
  get isAuthFailure(): boolean {
    return this.status === 401
  }

  /** 「不存在或无权访问」。★ 服务端刻意不区分这两者，文案也不能区分。 */
  get isNotFoundOrForbidden(): boolean {
    return this.status === 404 || this.status === 403
  }
}

// ---------------------------------------------------------------------------
// 令牌与失效回调
// ---------------------------------------------------------------------------

let accessToken: string | null = null

/**
 * access token 只放内存，**不落 localStorage**。
 *
 * 页面刷新后内存里的 token 会丢，但那没关系：refresh cookie 还在，
 * 启动时调一次 `refresh()` 就能拿回新的 access token（见 stores/auth.ts 的 bootstrap）。
 * 这个取舍把 XSS 能偷到的东西从「长期凭证」降级成「15 分钟内有效的一次性凭证」。
 */
export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function getAccessToken(): string | null {
  return accessToken
}

let authLostHandler: (() => void) | null = null

/** 注册「刷新也救不回来了」时的回调（store 用它清状态 + 跳登录页）。 */
export function setAuthLostHandler(fn: (() => void) | null): void {
  authLostHandler = fn
}

// ---------------------------------------------------------------------------
// 刷新去重
// ---------------------------------------------------------------------------

let refreshing: Promise<boolean> | null = null

/** 共享的刷新入口。并发调用只会真正刷新一次。 */
export function refreshShared(): Promise<boolean> {
  if (refreshing === null) {
    refreshing = doRefresh().finally(() => {
      refreshing = null
    })
  }
  return refreshing
}

async function doRefresh(): Promise<boolean> {
  try {
    const res = await fetch(`${BASE}/auth/refresh`, {
      method: 'POST',
      headers: { Accept: 'application/json' },
      // ★ 这一行是 refresh 能工作的全部原因：refresh token 在 Cookie 里。
      credentials: 'include',
    })
    if (!res.ok) {
      accessToken = null
      return false
    }
    const data = (await res.json()) as TokenResponse
    accessToken = data.access_token
    return true
  } catch {
    accessToken = null
    return false
  }
}

function authLost(): void {
  accessToken = null
  authLostHandler?.()
}

// ---------------------------------------------------------------------------
// 核心请求
// ---------------------------------------------------------------------------

interface RequestOptions {
  /** 内部标记：这次是刷新后的重试，不能再触发一次刷新。 */
  retried?: boolean
  signal?: AbortSignal
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  opts: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (accessToken !== null) headers['Authorization'] = `Bearer ${accessToken}`

  let res: Response
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
      ...(opts.signal ? { signal: opts.signal } : {}),
    })
  } catch (e) {
    // 网络层失败（离线、DNS、被代理拦）。抛成 ApiError 让调用方只有一种错误类型要处理。
    throw new ApiError(0, 'network', `网络请求失败：${e instanceof Error ? e.message : String(e)}`, true)
  }

  if (res.status === 401 && opts.retried !== true && !NO_AUTO_REFRESH.has(path)) {
    const ok = await refreshShared()
    if (!ok) {
      authLost()
      throw new ApiError(401, 'unauthenticated', '登录已失效，请重新登录')
    }
    return request<T>(method, path, body, { ...opts, retried: true })
  }

  if (res.status === 204) {
    return undefined as T
  }

  const text = await res.text()
  let parsed: unknown
  if (text.length > 0) {
    try {
      parsed = JSON.parse(text)
    } catch {
      throw new ApiError(
        res.status,
        'internal',
        res.ok ? '服务端返回了非 JSON 响应' : `服务端返回了非 JSON 错误响应 (HTTP ${res.status})`,
      )
    }
  }

  if (!res.ok) {
    const detail = extractError(parsed)
    throw new ApiError(res.status, detail.code, detail.message, detail.retryable)
  }
  return parsed as T
}

/** 从 `{error: {code, message}}` 里取错误信息（`internal/server/http.go: apiErrorBody`）。 */
function extractError(parsed: unknown): { code: string; message: string; retryable: boolean } {
  if (parsed !== null && typeof parsed === 'object' && 'error' in parsed) {
    const e = (parsed as { error: unknown }).error
    if (e !== null && typeof e === 'object') {
      const o = e as Record<string, unknown>
      return {
        code: typeof o['code'] === 'string' ? o['code'] : 'internal',
        message: typeof o['message'] === 'string' ? o['message'] : '请求失败',
        retryable: o['retryable'] === true,
      }
    }
  }
  return { code: 'internal', message: '请求失败', retryable: false }
}

// ---------------------------------------------------------------------------
// DTO（与 Go 侧 struct 逐字对齐，字段名保留 snake_case —— 它们直接来自服务端）
// ---------------------------------------------------------------------------

export interface TokenResponse {
  access_token: string
  token_type: string
  expires_in: number
}

export interface MeResponse {
  id: string
  email: string
  role: string
}

/** `internal/server/handler_device.go: deviceDTO`。 */
export interface DeviceDTO {
  id: string
  name: string
  platform: string
  arch: string
  agent_version: string
  online: boolean
  paired: boolean
  paired_at?: string
  last_seen_at?: string
  created_at: string
}

/** `pairPreviewResponse` —— 两阶段配对的核心（§10.4）。 */
export interface PairPreview {
  device_id: string
  name: string
  platform: string
  arch: string
  agent_version: string
  agent_ip: string
  already_paired: boolean
  expires_at: string
  /** Agent 此刻是否还挂着等确认。false 表示确认后 Agent 那边可能不会立刻收到通知。 */
  agent_online: boolean
}

/** `auditDTO`。 */
export interface AuditItem {
  id: string
  action: string
  result: string
  device_id?: string
  session_id?: string
  ip?: string
  user_agent?: string
  meta?: unknown
  created_at: string
}

export interface AuditPage {
  items: AuditItem[]
  next_cursor?: string
}

/** `POST /api/v1/ws-ticket` 的响应。 */
export interface WSTicketResponse {
  ticket: string
  expires_in: number
  /** 服务端支持的协议版本区间，用于提前拒绝不兼容的页面（§9.4）。 */
  protocol: { min: number; max: number }
}

// ---------------------------------------------------------------------------
// API 表面
// ---------------------------------------------------------------------------

export const api = {
  // ---- 认证 ----

  async register(email: string, password: string): Promise<TokenResponse> {
    const r = await request<TokenResponse>('POST', '/auth/register', { email, password })
    accessToken = r.access_token
    return r
  },

  async login(email: string, password: string): Promise<TokenResponse> {
    const r = await request<TokenResponse>('POST', '/auth/login', { email, password })
    accessToken = r.access_token
    return r
  },

  /**
   * 主动刷新（用于页面启动时用 Cookie 换回 access token）。
   * 失败返回 false 而不是抛错 —— 启动路径上「没登录」是正常状态，不是异常。
   */
  refresh(): Promise<boolean> {
    return refreshShared()
  },

  async logout(): Promise<void> {
    try {
      await request<void>('POST', '/auth/logout')
    } finally {
      // 无论服务端是否成功，本地状态都必须清掉 ——
      // 否则一次网络抖动会让用户卡在「点了登出但还登录着」的状态里。
      accessToken = null
    }
  },

  async changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await request<void>('POST', '/auth/password', {
      current_password: currentPassword,
      new_password: newPassword,
    })
  },

  me(): Promise<MeResponse> {
    return request<MeResponse>('GET', '/me')
  },

  installTicket(os: "windows" | "linux" | "darwin"): Promise<{ command: string; expires_in: number }> {
    return request("POST", "/agent-install-ticket", { os })
  },

  // ---- 设备 ----

  async listDevices(): Promise<DeviceDTO[]> {
    const r = await request<{ devices: DeviceDTO[] }>('GET', '/devices')
    return r.devices ?? []
  },

  getDevice(id: string): Promise<DeviceDTO> {
    return request<DeviceDTO>('GET', `/devices/${encodeURIComponent(id)}`)
  },

  renameDevice(id: string, name: string): Promise<DeviceDTO> {
    return request<DeviceDTO>('PATCH', `/devices/${encodeURIComponent(id)}`, { name })
  },

  async deleteDevice(id: string): Promise<void> {
    await request<void>('DELETE', `/devices/${encodeURIComponent(id)}`)
  },

  /** 设备详情页的会话缓存（Agent 上报过的历史会话）。 */
  async deviceSessions(id: string): Promise<SessionSummary[]> {
    const r = await request<{ sessions: SessionSummary[] }>(
      'GET',
      `/devices/${encodeURIComponent(id)}/sessions`,
    )
    return r.sessions ?? []
  },

  // ---- 配对（两阶段）----

  /** 第一步：提交配对码，拿回待确认设备的信息。这一步**不**产生绑定。 */
  pairPreview(code: string): Promise<PairPreview> {
    return request<PairPreview>('POST', '/devices/pair', { code })
  },

  /** 第二步：用户看过设备信息、确认是自己的机器之后，才真正绑定。 */
  async pairConfirm(code: string, name?: string): Promise<void> {
    await request<unknown>('POST', '/devices/pair/confirm', name ? { code, name } : { code })
  },

  // ---- 审计 ----

  audit(limit = 50, cursor?: string): Promise<AuditPage> {
    const qs = new URLSearchParams({ limit: String(limit) })
    if (cursor !== undefined && cursor !== '') qs.set('cursor', cursor)
    return request<AuditPage>('GET', `/audit?${qs.toString()}`)
  },

  // ---- WebSocket 票据 ----

  wsTicket(): Promise<WSTicketResponse> {
    return request<WSTicketResponse>('POST', '/ws-ticket')
  },

  // ---- 版本（无认证）----

  version(): Promise<{ protocol: { min: number; max: number }; [k: string]: unknown }> {
    return request<{ protocol: { min: number; max: number } }>('GET', '/version')
  },
}
