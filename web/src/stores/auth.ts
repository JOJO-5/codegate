/**
 * 认证状态。
 *
 * # access token 为什么不持久化
 *
 * 页面刷新后内存里的 access token 会丢 —— 这是**故意**的。
 * refresh token 在 HttpOnly Cookie 里，页面重新加载时用它换一个新的 access token
 * 即可（`bootstrap()`）。这样 XSS 能偷到的东西就只有「15 分钟内有效的一次性凭证」，
 * 而不是一个可以长期续命的凭据。
 *
 * 代价是每次冷启动都要多一次 `POST /auth/refresh` 往返 —— 几百毫秒，
 * 换来的是把长期凭证从 JS 可达范围内彻底移出去。这个交换很划算。
 */

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { ApiError, api, setAuthLostHandler, type MeResponse } from '../lib/api'
import { humanizeError } from '../lib/format'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<MeResponse | null>(null)

  /** 冷启动期间为 true：此时还不知道是否已登录，路由守卫必须等它。 */
  const booting = ref(true)

  const busy = ref(false)
  const error = ref<string | null>(null)

  const isLoggedIn = computed(() => user.value !== null)
  const email = computed(() => user.value?.email ?? '')

  /**
   * 冷启动：用 refresh cookie 尝试恢复会话。
   *
   * 失败是**正常路径**（用户第一次来、或者 Cookie 过期了），
   * 所以这里不设 error —— 否则新用户一进页面就看到一个红色错误条。
   *
   * ★ 必须幂等：路由守卫在每次导航时都可能调它，而 `POST /auth/refresh`
   *   是**轮换 + 重用检测**的 —— 并发刷新同一个令牌会被判定为重放攻击，
   *   触发整族吊销。所以这里用共享 Promise 把并发收敛成一次。
   */
  let bootstrapPromise: Promise<void> | null = null

  function bootstrap(): Promise<void> {
    if (bootstrapPromise === null) {
      bootstrapPromise = doBootstrap()
    }
    return bootstrapPromise
  }

  async function doBootstrap(): Promise<void> {
    booting.value = true
    try {
      const ok = await api.refresh()
      if (ok) {
        user.value = await api.me()
      }
    } catch {
      user.value = null
    } finally {
      booting.value = false
    }
  }

  async function login(mail: string, password: string): Promise<boolean> {
    busy.value = true
    error.value = null
    try {
      await api.login(mail, password)
      user.value = await api.me()
      return true
    } catch (e) {
      error.value = humanizeError(e)
      return false
    } finally {
      busy.value = false
    }
  }

  async function register(mail: string, password: string): Promise<boolean> {
    busy.value = true
    error.value = null
    try {
      await api.register(mail, password)
      user.value = await api.me()
      return true
    } catch (e) {
      error.value = humanizeError(e)
      return false
    } finally {
      busy.value = false
    }
  }

  async function logout(): Promise<void> {
    busy.value = true
    try {
      await api.logout()
    } catch {
      // api.logout 内部已经保证本地令牌被清掉；服务端失败不该挡住用户登出。
    } finally {
      user.value = null
      busy.value = false
    }
  }

  async function changePassword(currentPassword: string, newPassword: string): Promise<boolean> {
    busy.value = true
    error.value = null
    try {
      await api.changePassword(currentPassword, newPassword)
      return true
    } catch (e) {
      error.value = humanizeError(e)
      return false
    } finally {
      busy.value = false
    }
  }

  function clearError(): void {
    error.value = null
  }

  /**
   * 注册「刷新也救不回来」的回调。
   *
   * 触发场景：服务端把 refresh token 整族吊销了（检测到重用），
   * 或者管理员禁用了账号。此时 api 层会调用它，我们只需要把本地状态清空 ——
   * 路由守卫会自动把用户送回登录页。
   */
  setAuthLostHandler(() => {
    user.value = null
  })

  return {
    user,
    booting,
    busy,
    error,
    isLoggedIn,
    email,
    bootstrap,
    login,
    register,
    logout,
    changePassword,
    clearError,
  }
})

/** 供路由守卫复用：把 ApiError 的 401 判断收敛到一处。 */
export function isUnauthorized(e: unknown): boolean {
  return e instanceof ApiError && e.status === 401
}
