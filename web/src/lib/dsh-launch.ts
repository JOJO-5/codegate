import {ref} from 'vue'

export interface DSHWebStatus { online: boolean; state: 'offline' | 'not_open' | 'starting' | 'stopping' | 'forwarding' | 'unknown'; mobile_ui?: string }
interface Dependencies {
  status(id: string): Promise<DSHWebStatus>
  start(id: string): Promise<{url: string}>
  navigate(url: string): void
  describe(error: unknown): string
}

// A waiting page owns its navigation, not the device page which opened it.
// Leaving/changing account invalidates receipts without pretending to stop DSH.
export function createDSHLauncher(deps: Dependencies) {
  const busy=ref(false), error=ref(''), phase=ref('idle'), observed=ref<DSHWebStatus['state'] | ''>('')
  const compatibility=ref('unknown')
  let revision=0, device=''
  function invalidate() { revision++;busy.value=false;phase.value='idle' }
  async function inspect(id: string) {
    if (busy.value) return
    const current=++revision;device=id;busy.value=true;phase.value='checking';error.value=''
    try {
      const status=await deps.status(id)
      if (current===revision) { observed.value=status.state;compatibility.value=status.mobile_ui || 'unknown' }
    } catch (e) { if (current===revision) error.value=deps.describe(e) }
    finally {if (current===revision) {busy.value=false;phase.value='idle'}}
  }
  async function open(id: string) {
    if (busy.value && device===id) return
    const current=++revision;device=id;busy.value=true;error.value='';observed.value='';phase.value='checking'
    try {
      const status=await deps.status(id)
      if (current!==revision) return
      observed.value=status.state
      compatibility.value=status.mobile_ui || 'unknown'
      if (!status.online) throw Error('设备离线，请确认电脑开机且 Agent 在线后重新连接。')
      if (status.state==='stopping') throw Error('DSH Web 正在等待停止回执，请稍后核对状态再打开。')
      phase.value=status.state==='starting'?'starting':'connecting'
      const result=await deps.start(id)
      if (current!==revision) return
      const target=new URL(result.url)
      if (target.protocol!=='https:' || target.username || target.password || target.pathname!=='/' || !target.searchParams.get('cg_ticket')) throw Error('打开链接无效，请返回设备页核对 DSH 转发设置。')
      phase.value='opening'
      deps.navigate(target.href)
    } catch (e) { if (current===revision) { error.value=deps.describe(e);phase.value='error' } }
    finally { if (current===revision) busy.value=false }
  }
  return {busy,error,phase,observed,compatibility,open,inspect,invalidate}
}
