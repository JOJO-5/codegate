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
import { api, type PairPreview } from '../lib/api'

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
const installCommand = ref('')
const installBusy = ref(false)
const installError = ref('')
const installCopied = ref(false)

function chooseOS(os: OnboardingOS): void {
  selectedOS.value = os
  installCommand.value = ''
  installError.value = ''
}

async function generateInstallCommand(): Promise<void> {
  installBusy.value = true
  installError.value = ''
  installCommand.value = ''
  try {
    const os = selectedOS.value === 'macos' ? 'darwin' : selectedOS.value
    const result = await api.installTicket(os)
    installCommand.value = result.command
    installCopied.value = false
  } catch (e) {
    installError.value = humanizeError(e)
  } finally {
    installBusy.value = false
  }
}

async function copyInstallCommand(): Promise<void> {
  try {
    await navigator.clipboard.writeText(installCommand.value)
    installCopied.value = true
  } catch {
    installError.value = '复制失败，请手动选择命令。'
  }
}
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
              @click="chooseOS(os)"
            >
              {{ os === 'windows' ? 'Windows' : os === 'linux' ? 'Linux' : 'macOS' }}
            </button>
          </div>
        </div>

        <div class="stack">
          <button class="btn btn--primary" type="button" :disabled="installBusy" @click="generateInstallCommand">
            {{ installBusy ? '生成中…' : '生成从本 Server 下载 Agent 的命令' }}
          </button>
          <div v-if="installError" class="notice notice--err">{{ installError }}</div>
          <div v-if="installCommand" class="notice notice--info">
            仅限所选系统，10 分钟内使用一次。运行后配置工作目录和命令，再配对。
            <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">{{ installCommand }}</pre>
            <button class="btn btn--sm" type="button" @click="copyInstallCommand">{{ installCopied ? '已复制' : '复制安装命令' }}</button>
          </div>
        </div>

        <div v-if="selectedOS === 'windows'" class="notice notice--info">
          在目标 Windows 电脑运行上方命令安装 Agent。安装完成后先配置 Agent，再执行：
          <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">&amp; "$env:LOCALAPPDATA\CodeGate\bin\codegate-agent.exe" doctor
&amp; "$env:LOCALAPPDATA\CodeGate\bin\codegate-agent.exe" pair</pre>
          运行前先配置 <code class="mono">%APPDATA%\CodeGate\agent.json</code> 中的 Server 地址、工作目录和允许的命令。
          运行 <code class="mono">pair</code> 后，把输出的配对码填在下面。绑定完成后运行
          <code class="mono">&amp; "$env:LOCALAPPDATA\CodeGate\bin\codegate-agent.exe" run</code>。若希望开机后未解锁也能使用，可在仓库根目录执行
          <code class="mono">powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$env:LOCALAPPDATA\CodeGate\agent-autostart.ps1" install -AgentPath "$env:LOCALAPPDATA\CodeGate\bin\codegate-agent.exe"</code>
          （输入当前 Windows 账号密码，不是 PIN；先关闭手动运行的 Agent）。
          <a href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-DOCKER.md" target="_blank" rel="noopener noreferrer">查看完整配置示例</a>。
        </div>

        <div v-else class="notice notice--info">
          在目标 {{ selectedOS === 'linux' ? 'Linux' : 'macOS' }} 电脑运行上方命令安装 Agent，然后配置 Agent，执行：
          <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">~/.local/bin/codegate-agent doctor
~/.local/bin/codegate-agent pair</pre>
          先配置
          <code class="mono">{{ selectedOS === 'linux' ? '~/.config/codegate/agent.json' : '~/Library/Application Support/codegate/agent.json' }}</code>
          中的 Server 地址、允许目录和命令。配对后可运行
          <code class="mono">~/.local/bin/codegate-agent run</code>，或安装登录前自启：
          <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">CODEGATE_AGENT_BINARY="$HOME/.local/bin/codegate-agent" sh "$HOME/.local/share/codegate/agent-autostart.sh" install</pre>
          <a href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-AGENT-UNIX.md" target="_blank" rel="noopener noreferrer">查看 Linux/macOS Agent 完整配置</a>。
          <div class="small" style="margin-top: 10px">
            若要在这台机器另外部署 Server，可安装 Docker、Compose，执行 <code class="mono">gh auth login</code> 后运行：
            <pre class="mono" style="overflow-x: auto; white-space: pre-wrap; overflow-wrap: anywhere">{{ serverInstallCommand }}</pre>
            <button class="btn btn--sm" type="button" @click="copyServerCommand">
              {{ copied ? '已复制' : '复制 Server 安装命令' }}
            </button>
            <span v-if="copyError" role="alert">{{ copyError }}</span>
            <a href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-DOCKER.md" target="_blank" rel="noopener noreferrer">Server 部署指南</a>。
          </div>
        </div>
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
