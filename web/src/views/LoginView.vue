<script setup lang="ts">
/**
 * 登录 / 注册。
 *
 * 两件事值得说明：
 *
 * 1. **注册入口是条件显示的，但前端不做判断。**
 *    服务端只在「系统里一个用户都没有」或 `allow_signup: true` 时接受注册。
 *    前端无法可靠地知道这个条件（那本身也是一个信息泄露面），
 *    所以这里始终显示切换按钮，让服务端去拒绝 —— 拒绝信息会如实展示。
 *    比"藏起来但不知道为什么"要好。
 *
 * 2. **登录失败的文案由服务端决定。**
 *    服务端刻意让「邮箱不存在」和「密码错误」返回**完全相同**的响应，
 *    以防账号枚举。前端绝不能自作聪明地补一句"该邮箱未注册"。
 */

import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()

const mode = ref<'login' | 'register'>('login')
const email = ref('')
const password = ref('')
const confirmPassword = ref('')
const localError = ref<string | null>(null)

const isRegister = computed(() => mode.value === 'register')

const mismatch = computed(
  () => isRegister.value && confirmPassword.value !== '' && password.value !== confirmPassword.value,
)

const canSubmit = computed(() => {
  if (auth.busy) return false
  if (email.value.trim() === '' || password.value === '') return false
  if (isRegister.value && password.value !== confirmPassword.value) return false
  return true
})

const shownError = computed(() => localError.value ?? auth.error)

async function submit(): Promise<void> {
  localError.value = null
  auth.clearError()

  if (isRegister.value && password.value !== confirmPassword.value) {
    localError.value = '两次输入的密码不一致'
    return
  }

  const mail = email.value.trim()
  const ok = isRegister.value
    ? await auth.register(mail, password.value)
    : await auth.login(mail, password.value)

  if (!ok) return

  const next = typeof route.query['next'] === 'string' ? route.query['next'] : '/projects'
  await router.replace(next)
}

function toggleMode(): void {
  mode.value = isRegister.value ? 'login' : 'register'
  localError.value = null
  confirmPassword.value = ''
  auth.clearError()
}
</script>

<template>
  <div class="login">
    <div class="login__box">
      <div class="login__brand">
        <div class="brand">CodeGate</div>
        <div class="faint small" style="margin-top: 6px">
          在手机或浏览器上操作家里那台电脑的终端
        </div>
      </div>

      <div class="card">
        <form class="stack" @submit.prevent="submit">
          <div class="field">
            <label for="email">邮箱</label>
            <input
              id="email"
              v-model="email"
              type="email"
              autocomplete="username"
              autocapitalize="none"
              spellcheck="false"
              placeholder="you@example.com"
              :disabled="auth.busy"
            />
          </div>

          <div class="field">
            <label for="password">密码</label>
            <input
              id="password"
              v-model="password"
              type="password"
              :autocomplete="isRegister ? 'new-password' : 'current-password'"
              placeholder="至少 8 位"
              :disabled="auth.busy"
            />
          </div>

          <div v-if="isRegister" class="field">
            <label for="confirm">确认密码</label>
            <input
              id="confirm"
              v-model="confirmPassword"
              type="password"
              autocomplete="new-password"
              :disabled="auth.busy"
            />
            <span v-if="mismatch" class="field__hint" style="color: var(--err)">
              两次输入的密码不一致
            </span>
          </div>

          <div v-if="shownError" class="notice notice--err">{{ shownError }}</div>

          <button class="btn btn--primary" type="submit" :disabled="!canSubmit">
            <span v-if="auth.busy" class="spinner" />
            {{ isRegister ? '注册并登录' : '登录' }}
          </button>
        </form>
      </div>

      <div class="center small" style="margin-top: 14px">
        <button class="btn btn--ghost btn--sm" type="button" @click="toggleMode">
          {{ isRegister ? '已有账号？去登录' : '第一次使用？创建账号' }}
        </button>
      </div>

      <p v-if="isRegister" class="faint small center" style="margin-top: 10px">
        注册仅在实例尚无任何用户时开放；已有用户后需管理员开启 allow_signup。
      </p>
    </div>
  </div>
</template>
