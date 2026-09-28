/// <reference types="vite/client" />

// vue-tsc 能自己理解 .vue，但 tsc（和编辑器的部分插件）需要这个声明兜底。
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>
  export default component
}

// xterm 的 addon 包没有随包发 .d.ts 的只有极少数版本，这里不预先声明 ——
// 声明了反而会掩盖「包名写错」这类真问题。真缺了会在 typecheck 阶段报出来。
