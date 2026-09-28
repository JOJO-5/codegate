/**
 * WebSocket 连接单例。
 *
 * 整个应用只有**一条**到服务端的 WS 连接 —— 这是服务端的设计决定的
 * （`/ws/client` 是每用户一条，多会话在同一条连接上靠 `session_id` 路由）。
 *
 * # 为什么用 store 而不是模块级单例
 *
 * 因为连接状态要驱动 UI（顶栏的连接指示灯、终端页的"正在重连…"遮罩）。
 * 模块级单例要把状态变化手动桥接进 Vue 的响应式系统，
 * 而 store 天然就在里面。
 *
 * # 为什么用 shallowRef 持有 client
 *
 * WSClient 内部有 Map、定时器、WebSocket 实例。让 Vue 深度代理它
 * 会带来两个后果：一是每次读 `client.value.pending` 都触发依赖收集，
 * 二是 WebSocket 这种宿主对象被 Proxy 包住之后，某些浏览器里
 * 方法调用会丢 this。浅引用正好：我们只关心「client 换没换」。
 */

import { computed, ref, shallowRef } from 'vue'
import { defineStore } from 'pinia'
import { WSClient, type WSState } from '../lib/ws'
import type { DecodedFrame, Envelope, MessageType } from '../lib/protocol'
import { FrameType } from '../lib/protocol'

type ControlHandler = (env: Envelope) => void
type FrameHandler = (frame: DecodedFrame) => void

export const useConnStore = defineStore('conn', () => {
  const state = ref<WSState>('idle')
  const detail = ref<string>('')
  const ready = ref(false)
  const fatal = ref<string | null>(null)
  /** 重连成功次数，用于让终端页知道「该恢复 attach 了」。 */
  const reconnectCount = ref(0)

  const controlHandlers = new Set<ControlHandler>()
  const frameHandlers = new Set<FrameHandler>()

  const client = shallowRef<WSClient | null>(null)

  const isOpen = computed(() => state.value === 'open')
  /** 顶栏指示灯的三态：ok（连上）/ warn（在重连）/ err（挂了）。 */
  const health = computed<'ok' | 'warn' | 'err' | 'idle'>(() => {
    if (fatal.value !== null) return 'err'
    if (state.value === 'open') return 'ok'
    if (state.value === 'connecting' || state.value === 'reconnecting') return 'warn'
    return 'idle'
  })

  const healthText = computed(() => {
    if (fatal.value !== null) return fatal.value
    switch (state.value) {
      case 'open':
        return '已连接'
      case 'connecting':
        return '连接中…'
      case 'reconnecting':
        return detail.value ? `正在重连…（${detail.value}）` : '正在重连…'
      case 'closed':
        return detail.value ? `已断开：${detail.value}` : '已断开'
      default:
        return '未连接'
    }
  })

  function ensure(): WSClient {
    if (client.value === null) {
      client.value = new WSClient({
        onState: (s, d) => {
          state.value = s
          detail.value = d ?? ''
        },
        onReady: (reconnect) => {
          ready.value = true
          if (reconnect) reconnectCount.value++
        },
        onFatal: (reason) => {
          ready.value = false
          fatal.value = reason
        },
        onControl: (env) => {
          for (const h of controlHandlers) h(env)
        },
        onFrame: (frame) => {
          for (const h of frameHandlers) h(frame)
        },
      })
    }
    return client.value
  }

  function connect(): void {
    fatal.value = null
    ensure().connect()
  }

  /** 主动断开（登出时调用）。 */
  function disconnect(): void {
    client.value?.dispose()
    client.value = null
    ready.value = false
    fatal.value = null
    state.value = 'closed'
    detail.value = ''
    reconnectCount.value = 0
  }

  /**
   * 订阅控制消息。返回取消订阅函数 —— **组件卸载时必须调用**，
   * 否则路由来回切换会累积监听器，而每个监听器都闭包持有一个组件实例。
   */
  function onControl(h: ControlHandler): () => void {
    controlHandlers.add(h)
    return () => {
      controlHandlers.delete(h)
    }
  }

  /** 订阅二进制帧。同样必须取消订阅。 */
  function onFrame(h: FrameHandler): () => void {
    frameHandlers.add(h)
    return () => {
      frameHandlers.delete(h)
    }
  }

  function request<T = unknown>(
    type: MessageType,
    payload: unknown,
    sessionId?: string,
  ): Promise<Envelope<T>> {
    return ensure().request<T>(type, payload, sessionId)
  }

  function notify(type: MessageType, payload: unknown, sessionId?: string): void {
    client.value?.notify(type, payload, sessionId)
  }

  function sendFrame(
    type: FrameType,
    flags: number,
    streamId: string,
    payload: Uint8Array,
  ): void {
    client.value?.sendFrame(type, flags, streamId, payload)
  }

  return {
    state,
    detail,
    ready,
    fatal,
    reconnectCount,
    isOpen,
    health,
    healthText,
    connect,
    disconnect,
    onControl,
    onFrame,
    request,
    notify,
    sendFrame,
  }
})
