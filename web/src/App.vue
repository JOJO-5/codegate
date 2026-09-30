<script setup lang="ts">
/**
 * 应用外壳。
 *
 * 负责两件事，都在这里做而不是散在各页面：
 *   1. 顶栏（导航 + 连接指示灯 + 登出）
 *   2. **WS 连接的生命周期** —— 登录就连、登出就断。
 *      只有一个地方决定什么时候连，就不会出现"两个页面各连一条"的问题
 *      （服务端对同一用户虽然允许多条，但每条都要独立 attach，纯浪费）。
 */

import { computed, watch } from 'vue'
import { RouterLink, RouterView, useRoute, useRouter } from 'vue-router'
import { useAuthStore } from './stores/auth'
import { useConnStore } from './stores/conn'

const auth = useAuthStore()
const conn = useConnStore()
const route = useRoute()
const router = useRouter()

/**
 * 终端页和登录页不要外壳。
 *
 * 终端页尤其重要：在手机上，一条 44px 的顶栏会直接吃掉一行终端输出，
 * 而移动端的屏幕高度是最稀缺的资源。
 */
const showChrome = computed(() => route.meta['chrome'] !== false)

watch(
  () => auth.isLoggedIn,
  (loggedIn) => {
    if (loggedIn) {
      conn.connect()
    } else {
      conn.disconnect()
    }
  },
  { immediate: true },
)

async function onLogout(): Promise<void> {
  // 先断 WS 再登出：反过来的话，服务端吊销会话时连接还活着，
  // 会收到一串 4401 关闭，在控制台留下一堆看起来像故障的日志。
  conn.disconnect()
  await auth.logout()
  await router.push({ name: 'login' })
}
</script>

<template>
  <div :class="showChrome ? 'shell' : ''">
    <header v-if="showChrome" class="topbar">
      <RouterLink to="/devices" class="brand">CodeGate</RouterLink>

      <nav class="nav">
        <RouterLink to="/devices">设备</RouterLink>
        <RouterLink to="/settings">设置</RouterLink>
      </nav>

      <div class="grow" />

      <span class="health" :class="`health--${conn.health}`" :title="conn.healthText">
        <span class="health__dot" />
        <span>{{ conn.healthText }}</span>
      </span>

      <button class="btn btn--ghost btn--sm" type="button" @click="onLogout">登出</button>
    </header>

    <RouterView :key="route.fullPath" />
  </div>
</template>
