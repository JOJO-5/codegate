<script setup lang="ts">
/**
 * 设置：账号信息、改密码、审计日志。
 *
 * # 改密码之后要做什么（这里刻意不做的那件事）
 *
 * 服务端的 `handleChangePassword` 会吊销该用户的**全部** refresh token
 * （改密码的语义就是"把其他设备踢下去"）。但**当前**这个会话的
 * access token 仍在有效期内，所以页面不会立刻掉线。
 *
 * 前端这里不主动登出：那会让用户困惑（"我改个密码怎么被踢了"）。
 * 正确的行为是改完就正常用，下次 access token 过期时用旧 refresh
 * 刷新失败 → 自动跳登录页。这恰好就是"改密码把别人踢下线"的预期体验。
 */

import { computed, onMounted, ref } from 'vue'
import { useAuthStore } from '../stores/auth'
import { useConnStore } from '../stores/conn'
import { ApiError, api, type AuditItem } from '../lib/api'
import { absoluteTime, humanizeError } from '../lib/format'

const auth = useAuthStore()
const conn = useConnStore()

// ---- 改密码 ----
const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')
const pwBusy = ref(false)
const pwOk = ref(false)
const pwError = ref<string | null>(null)

const pwMismatch = computed(
  () => confirmPassword.value !== '' && newPassword.value !== confirmPassword.value,
)

const canSubmitPw = computed(() => {
  if (pwBusy.value) return false
  if (currentPassword.value === '' || newPassword.value === '') return false
  if (newPassword.value !== confirmPassword.value) return false
  return true
})

async function submitPassword(): Promise<void> {
  pwOk.value = false
  pwError.value = null
  auth.clearError()

  if (newPassword.value !== confirmPassword.value) {
    pwError.value = '两次输入的新密码不一致'
    return
  }

  pwBusy.value = true
  const ok = await auth.changePassword(currentPassword.value, newPassword.value)
  pwBusy.value = false

  if (ok) {
    pwOk.value = true
    currentPassword.value = ''
    newPassword.value = ''
    confirmPassword.value = ''
  } else {
    pwError.value = auth.error ?? '修改失败'
  }
}

// ---- 审计日志 ----
const auditItems = ref<AuditItem[]>([])
const auditCursor = ref<string | undefined>(undefined)
const auditLoading = ref(false)
const auditError = ref<string | null>(null)
const auditDone = ref(false)

async function loadAudit(more = false): Promise<void> {
  if (auditLoading.value) return
  auditLoading.value = true
  auditError.value = null
  try {
    const page = await api.audit(50, more ? auditCursor.value : undefined)
    auditItems.value = more ? [...auditItems.value, ...page.items] : page.items
    auditCursor.value = page.next_cursor
    auditDone.value = page.next_cursor === undefined || page.items.length === 0
  } catch (e) {
    auditError.value = humanizeError(e)
    // 游标不合法（服务端会返回 invalid_payload）说明链接/状态过期了，
    // 直接重来一次比让用户卡住好。
    if (e instanceof ApiError && e.code === 'invalid_payload') {
      auditCursor.value = undefined
      auditDone.value = false
    }
  } finally {
    auditLoading.value = false
  }
}

onMounted(() => {
  void loadAudit(false)
})

/** 把审计动作翻成人话。未知动作原样显示 —— 总比显示"未知"信息多。 */
function actionLabel(action: string): string {
  const map: Record<string, string> = {
    auth_register: '注册账号',
    auth_login: '登录',
    auth_login_failed: '登录失败',
    auth_logout: '登出',
    auth_refresh: '刷新令牌',
    auth_password_changed: '修改密码',
    device_pair_preview: '配对：查看设备',
    device_pair_confirm: '配对：确认绑定',
    device_pair_failed: '配对失败',
    device_rename: '重命名设备',
    device_delete: '解绑设备',
    session_create: '新建会话',
    session_attach: '接管会话',
    session_close: '终止会话',
    agent_connect: 'Agent 上线',
    agent_auth_failed: 'Agent 认证失败',
    ws_ticket: '签发 WS 票据',
  }
  return map[action] ?? action
}
</script>

<template>
  <main class="page">
    <h1>设置</h1>

    <!-- ---- 账号 ---- -->
    <div class="card">
      <div class="card__title">
        <h2>账号</h2>
        <span class="badge" :class="`badge--${conn.health}`">{{ conn.healthText }}</span>
      </div>
      <table class="table">
        <tbody>
          <tr>
            <th style="width: 96px">邮箱</th>
            <td class="mono">{{ auth.user?.email ?? '—' }}</td>
          </tr>
          <tr>
            <th>角色</th>
            <td>{{ auth.user?.role ?? '—' }}</td>
          </tr>
          <tr>
            <th>用户 ID</th>
            <td class="mono small">{{ auth.user?.id ?? '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- ---- 改密码 ---- -->
    <div class="card">
      <div class="card__title">
        <h2>修改密码</h2>
      </div>

      <form class="stack" @submit.prevent="submitPassword">
        <div class="field">
          <label for="cur">当前密码</label>
          <input
            id="cur"
            v-model="currentPassword"
            type="password"
            autocomplete="current-password"
            :disabled="pwBusy"
          />
        </div>

        <div class="field">
          <label for="new">新密码</label>
          <input
            id="new"
            v-model="newPassword"
            type="password"
            autocomplete="new-password"
            placeholder="至少 8 位"
            :disabled="pwBusy"
          />
        </div>

        <div class="field">
          <label for="new2">确认新密码</label>
          <input
            id="new2"
            v-model="confirmPassword"
            type="password"
            autocomplete="new-password"
            :disabled="pwBusy"
          />
          <span v-if="pwMismatch" class="field__hint" style="color: var(--err)">
            两次输入的新密码不一致
          </span>
        </div>

        <div v-if="pwError" class="notice notice--err">{{ pwError }}</div>
        <div v-if="pwOk" class="notice notice--info">
          密码已修改。其他设备上的登录会在它们的 access token 过期后失效。
        </div>

        <div class="row">
          <button class="btn btn--primary" type="submit" :disabled="!canSubmitPw">
            <span v-if="pwBusy" class="spinner" />
            修改密码
          </button>
        </div>
      </form>
    </div>

    <!-- ---- 审计日志 ---- -->
    <div class="card">
      <div class="card__title">
        <h2>审计日志</h2>
        <button class="btn btn--sm" type="button" :disabled="auditLoading" @click="loadAudit(false)">
          <span v-if="auditLoading" class="spinner" />
          刷新
        </button>
      </div>

      <div v-if="auditError" class="notice notice--err" style="margin-bottom: 12px">
        {{ auditError }}
      </div>

      <div v-if="auditItems.length === 0 && !auditLoading" class="empty">还没有记录。</div>

      <table v-else class="table">
        <thead>
          <tr>
            <th>时间</th>
            <th>动作</th>
            <th>结果</th>
            <th>来源 IP</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in auditItems" :key="a.id">
            <td class="mono small nowrap">{{ absoluteTime(a.created_at) }}</td>
            <td>
              {{ actionLabel(a.action) }}
              <div v-if="a.session_id" class="faint small mono">
                会话 {{ a.session_id.slice(0, 8) }}
              </div>
            </td>
            <td>
              <span class="badge" :class="a.result === 'ok' || a.result === 'success' ? 'badge--ok' : 'badge--err'">
                {{ a.result }}
              </span>
            </td>
            <td class="mono small">{{ a.ip || '—' }}</td>
          </tr>
        </tbody>
      </table>

      <div v-if="!auditDone && auditItems.length > 0" class="row" style="margin-top: 12px">
        <button class="btn btn--sm" type="button" :disabled="auditLoading" @click="loadAudit(true)">
          <span v-if="auditLoading" class="spinner" />
          加载更多
        </button>
      </div>
    </div>
  </main>
</template>
