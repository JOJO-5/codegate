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
type OnboardingOS = 'windows' | 'linux' | 'macos'
const onboardingOS: OnboardingOS[] = ['windows', 'linux', 'macos']
const selectedOS = ref<OnboardingOS>('windows')
const copied = ref(false)
const copyError = ref('')
const serverInstallCommand =
  'bash -o pipefail -c "gh api -H \'Accept: application/vnd.github.raw+json\' repos/JOJO-5/codegate/contents/deploy/install-compose.sh | sh"'

async function copyServerCommand(): Promise<void> {
  copied.value = false
  copyError.value = ''
  try {
    await navigator.clipboard.writeText(serverInstallCommand)
    copied.value = true
  } catch {
    copyError.value = '复制失败，请手动选择命令复制。'
  }
}

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
        在下方选择目标电脑的系统，按步骤启动 Agent 后填写配对码。
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

      <div v-if="preview === null" class="stack">
        <div class="field">
          <span>目标电脑的系统</span>
          <div class="row" role="group" aria-label="目标电脑的系统">
            <button
              v-for="os in onboardingOS"
              :key="os"
              class="btn btn--sm"
              type="button"
              :aria-pressed="selectedOS === os"
              :class="selectedOS === os ? 'btn--primary' : ''"
              @click="selectedOS = os"
            >
              {{ os === 'windows' ? 'Windows' : os === 'linux' ? 'Linux' : 'macOS' }}
            </button>
          </div>
        </div>

        <div v-if="selectedOS === 'windows'" class="notice notice--info">
          在目标 Windows 电脑上从
          <a href="https://github.com/JOJO-5/codegate" target="_blank" rel="noopener noreferrer">私有仓库</a>
          在仓库根目录构建原生 Agent：
          <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">go build -o bin/codegate-agent.exe ./cmd/codegate-agent
.\bin\codegate-agent.exe doctor
.\bin\codegate-agent.exe pair</pre>
          运行前先配置 <code class="mono">%APPDATA%\CodeGate\agent.json</code> 中的 Server 地址、工作目录和允许的命令。
          运行 <code class="mono">pair</code> 后，把输出的配对码填在下面。绑定完成后运行
          <code class="mono">.\bin\codegate-agent.exe run</code>。
          <a href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-DOCKER.md" target="_blank" rel="noopener noreferrer">查看完整配置示例</a>。
        </div>

        <div v-else class="notice notice--warn">
          {{ selectedOS === 'linux' ? 'Linux' : 'macOS' }} 目前还不能作为被控电脑接入：Agent 的终端功能只在 Windows 上实现。
          如果要在这台机器部署 Server，可以先安装并启动 Docker、Compose，运行 <code class="mono">gh auth login</code>，
          再执行下面的命令。私有仓库和镜像需要读取权限。
          <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">{{ serverInstallCommand }}</pre>
          <button class="btn btn--sm" type="button" @click="copyServerCommand">
            {{ copied ? '已复制' : '复制 Server 安装命令' }}
          </button>
          <span v-if="copyError" role="alert">{{ copyError }}</span>
          <div class="small" style="margin-top: 8px">
            这条命令部署的是 Server，不会在 Linux/macOS 上安装可配对的 Agent。
            公网域名及 HTTPS 设置见
            <a href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-DOCKER.md" target="_blank" rel="noopener noreferrer">部署指南</a>。
          </div>
        </div>
      </div>

      <!-- 第一步：输入配对码 -->
      <form v-if="preview === null && selectedOS === 'windows'" class="stack" @submit.prevent="submitCode">
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
      <div v-else-if="preview !== null" class="stack">
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
