<script setup lang="ts">
/**
 * 设备列表 + 配对入口。
 *
 * # 配对为什么要两步
 *
 * 猜中一个配对码**不等于**能绑定成功。用户提交码之后先看到
 * 「这台机器叫什么、什么平台、从哪个 IP 连过来的」，
 * 确认是自己的机器才点绑定（§10.4）。
 *
 * 没有这一步的话，猜中码 = 直接把别人的电脑绑到自己账号上。
 * 所以这里**不做**「输完码直接绑定」的快捷路径 —— 少一次点击的代价
 * 是把一个安全设计整个绕过去。
 */

import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useDevicesStore } from '../stores/devices'
import { humanizeError, platformGlyph, platformLabel, relativeTime, toMs } from '../lib/format'
import type { PairPreview } from '../lib/api'

const devices = useDevicesStore()

// ---- 配对 ----
const pairCode = ref('')
const preview = ref<PairPreview | null>(null)
const pairName = ref('')
const pairError = ref<string | null>(null)
const pairBusy = ref(false)

onMounted(() => {
  void devices.load()
})

async function submitCode(): Promise<void> {
  const code = pairCode.value.trim()
  if (code === '') return

  pairBusy.value = true
  pairError.value = null
  try {
    const p = await devices.previewPair(code)
    preview.value = p
    pairName.value = p.name
  } catch (e) {
    pairError.value = humanizeError(e)
  } finally {
    pairBusy.value = false
  }
}

function cancelPreview(): void {
  preview.value = null
  pairError.value = null
}

async function confirmPair(): Promise<void> {
  if (preview.value === null) return
  pairBusy.value = true
  pairError.value = null
  try {
    const name = pairName.value.trim()
    await devices.confirmPair(pairCode.value.trim(), name === '' ? undefined : name)
    preview.value = null
    pairCode.value = ''
    pairName.value = ''
  } catch (e) {
    pairError.value = humanizeError(e)
  } finally {
    pairBusy.value = false
  }
}

function lastSeenText(d: { online: boolean; last_seen_at?: string }): string {
  if (d.online) return '在线'
  const ms = toMs(d.last_seen_at)
  return ms === null ? '从未上线' : `最后在线 ${relativeTime(ms)}`
}
</script>

<template>
  <main class="page">
    <div class="row row--between" style="margin-bottom: 16px">
      <h1>设备</h1>
      <button class="btn btn--sm" type="button" :disabled="devices.loading" @click="devices.load(true)">
        <span v-if="devices.loading" class="spinner" />
        刷新
      </button>
    </div>

    <div v-if="devices.error" class="notice notice--err" style="margin-bottom: 14px">
      {{ devices.error }}
    </div>

    <!-- ---- 设备列表 ---- -->
    <div v-if="devices.loading && devices.items.length === 0" class="empty">
      <span class="spinner" />
    </div>

    <div v-else-if="devices.items.length === 0" class="empty">
      <div>还没有绑定任何设备。</div>
      <div class="small" style="margin-top: 6px">
        在那台电脑上运行 <code class="mono">codegate-agent pair</code> 获取配对码，然后填到下面。
      </div>
    </div>

    <div v-else class="list">
      <RouterLink
        v-for="d in devices.sorted"
        :key="d.id"
        class="item"
        :to="{ name: 'device', params: { id: d.id } }"
      >
        <span style="font-size: 18px" aria-hidden="true">{{ platformGlyph(d.platform) }}</span>
        <span class="item__main">
          <span class="item__title">
            {{ d.name }}
            <span class="badge" :class="d.online ? 'badge--ok' : 'badge--idle'">
              {{ d.online ? '在线' : '离线' }}
            </span>
          </span>
          <span class="item__meta">
            {{ platformLabel(d.platform) }}/{{ d.arch }} · agent {{ d.agent_version || '—' }} ·
            {{ lastSeenText(d) }}
          </span>
        </span>
        <span class="faint">›</span>
      </RouterLink>
    </div>

    <!-- ---- 配对 ---- -->
    <div class="card" style="margin-top: 18px">
      <div class="card__title">
        <h2>添加设备</h2>
      </div>

      <!-- 第一步：输入配对码 -->
      <form v-if="preview === null" class="stack" @submit.prevent="submitCode">
        <div class="field">
          <label for="paircode">配对码</label>
          <input
            id="paircode"
            v-model="pairCode"
            class="mono"
            type="text"
            inputmode="text"
            autocapitalize="characters"
            autocorrect="off"
            spellcheck="false"
            placeholder="XXXX-XXXX"
            :disabled="pairBusy"
          />
          <span class="field__hint">
            在目标电脑上运行 <code class="mono">codegate-agent pair</code> 获取，10 分钟内有效，只能用一次。
          </span>
        </div>

        <div v-if="pairError" class="notice notice--err">{{ pairError }}</div>

        <div class="row">
          <button class="btn btn--primary" type="submit" :disabled="pairBusy || pairCode.trim() === ''">
            <span v-if="pairBusy" class="spinner" />
            下一步
          </button>
        </div>
      </form>

      <!-- 第二步：确认设备信息 -->
      <div v-else class="stack">
        <div class="notice notice--info">
          请确认下面这台机器是你自己的。不是你的就取消 —— 猜中配对码不该等于能绑定别人的电脑。
        </div>

        <table class="table">
          <tbody>
            <tr>
              <th style="width: 96px">设备名</th>
              <td>{{ preview.name || '（未命名）' }}</td>
            </tr>
            <tr>
              <th>平台</th>
              <td>{{ platformLabel(preview.platform) }}/{{ preview.arch }}</td>
            </tr>
            <tr>
              <th>Agent 版本</th>
              <td class="mono">{{ preview.agent_version || '—' }}</td>
            </tr>
            <tr>
              <th>来源 IP</th>
              <td class="mono">{{ preview.agent_ip || '—' }}</td>
            </tr>
            <tr>
              <th>设备 ID</th>
              <td class="mono">{{ preview.device_id }}</td>
            </tr>
          </tbody>
        </table>

        <div v-if="!preview.agent_online" class="notice notice--warn">
          Agent 当前不在线。绑定会成功，但对方不会立刻收到通知 —— 它下次上线时才会同步。
        </div>
        <div v-if="preview.already_paired" class="notice notice--warn">
          这台设备已经绑定过了。继续会在你的账号下更新它的公钥与名称。
        </div>

        <div class="field">
          <label for="pairname">显示名（可改）</label>
          <input id="pairname" v-model="pairName" type="text" :disabled="pairBusy" />
        </div>

        <div v-if="pairError" class="notice notice--err">{{ pairError }}</div>

        <div class="row">
          <button class="btn btn--primary" type="button" :disabled="pairBusy" @click="confirmPair">
            <span v-if="pairBusy" class="spinner" />
            确认绑定
          </button>
          <button class="btn" type="button" :disabled="pairBusy" @click="cancelPreview">取消</button>
        </div>
      </div>
    </div>
  </main>
</template>
