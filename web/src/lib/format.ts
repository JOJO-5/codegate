/**
 * 展示层的小工具：时间、平台、状态、字节数、错误文案。
 *
 * 全部是纯函数，放一个文件就够了 —— 为这些各建一个模块
 * 只会让 import 行变长，不会让代码变清楚。
 */

import { ApiError } from './api'
import { WSError } from './ws'
import type { SessionStatus } from './protocol'

/**
 * 相对时间（"3 分钟前"）。
 *
 * 用 `Intl.RelativeTimeFormat` 而不是手拼字符串：中文的"前/后"、
 * 英文的"ago/in"，以及各种语言的复数规则都由平台处理，
 * 手写一遍等于把本地化的坑重新踩一次。
 */
const rtf = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })

const UNITS: Array<[Intl.RelativeTimeFormatUnit, number]> = [
  ['second', 1000],
  ['minute', 60_000],
  ['hour', 3_600_000],
  ['day', 86_400_000],
  ['week', 604_800_000],
  ['month', 2_629_800_000],
  ['year', 31_557_600_000],
]

export function relativeTime(ms: number, now: number = Date.now()): string {
  const diff = ms - now
  const abs = Math.abs(diff)
  if (abs < 5_000) return '刚刚'

  // 从大到小找第一个能整除的量级
  let chosen: [Intl.RelativeTimeFormatUnit, number] = UNITS[0]
  for (const u of UNITS) {
    if (abs >= u[1]) chosen = u
  }
  return rtf.format(Math.round(diff / chosen[1]), chosen[0])
}

/** 绝对时间（本地时区，精确到秒）。 */
export function absoluteTime(input: number | string | null | undefined): string {
  if (input === null || input === undefined || input === '') return '—'
  const ms = typeof input === 'number' ? input : Date.parse(input)
  if (Number.isNaN(ms)) return '—'
  return new Date(ms).toLocaleString('zh-CN', { hour12: false })
}

/** 解析后端返回的时间：可能是 RFC3339 字符串，也可能是 Unix 毫秒。 */
export function toMs(input: number | string | null | undefined): number | null {
  if (input === null || input === undefined || input === '') return null
  if (typeof input === 'number') return input
  const ms = Date.parse(input)
  return Number.isNaN(ms) ? null : ms
}

/** 平台显示名。后端给的是 `windows` / `linux` / `darwin`。 */
export function platformLabel(platform: string): string {
  switch (platform.toLowerCase()) {
    case 'windows':
      return 'Windows'
    case 'linux':
      return 'Linux'
    case 'darwin':
    case 'macos':
      return 'macOS'
    default:
      return platform || '未知'
  }
}

/** 平台图标（纯文本，不引图标库 —— 规格要求不引 UI 库）。 */
export function platformGlyph(platform: string): string {
  switch (platform.toLowerCase()) {
    case 'windows':
      return '⊞'
    case 'linux':
      return '🐧'
    case 'darwin':
    case 'macos':
      return ''
    default:
      return '▢'
  }
}

/** 会话状态的中文标签。 */
export function statusLabel(s: SessionStatus | string): string {
  switch (s) {
    case 'starting':
      return '启动中'
    case 'running':
      return '运行中'
    case 'detached':
      return '后台运行'
    case 'exited':
      return '已退出'
    case 'failed':
      return '启动失败'
    case 'terminated':
      return '已终止'
    default:
      return s
  }
}

/** 会话状态对应的 CSS 类后缀（见 styles.css 的 .badge--*）。 */
export function statusKind(s: SessionStatus | string): string {
  switch (s) {
    case 'starting':
      return 'warn'
    case 'running':
      return 'ok'
    case 'detached':
      return 'idle'
    case 'exited':
      return 'idle'
    case 'failed':
      return 'err'
    case 'terminated':
      return 'err'
    default:
      return 'idle'
  }
}

/** 字节数。 */
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`
}

/**
 * 把任意异常翻译成可以直接给用户看的一句话。
 *
 * ★ 这里有一条不能违反的规则：**服务端刻意不区分「不存在」和「无权访问」**
 *   （两端返回同状态码、同错误码、同文案），为的是消除存在性预言机。
 *   所以前端也不能自作聪明地把它拆开 —— 拆开就等于把服务端堵上的信息
 *   从 UI 上又漏出去。
 */
export function humanizeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 0) return '网络不可用，请检查连接后重试'
    if (e.status === 401) return '登录已失效，请重新登录'
    if (e.status === 429) return '操作过于频繁，请稍后再试'
    if (e.status >= 500) return '服务端出错了，请稍后重试'
    return e.message || `请求失败 (HTTP ${e.status})`
  }
  if (e instanceof WSError) {
    return e.message || '连接异常'
  }
  if (e instanceof Error) {
    return e.message
  }
  return String(e)
}
