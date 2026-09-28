/**
 * 应用入口。
 *
 * xterm 的样式表在这里**全局**引一次，而不是在终端页里引：
 * 组件级 `import '...css'` 在 Vite 里会被提取成独立的 CSS chunk，
 * 但 xterm 的 `.xterm` 规则必须在对它做 `open()` 之前就生效 ——
 * 否则首帧会按错误的行高计算，然后被 FitAddon 纠正，
 * 视觉上就是终端"跳"一下。
 */

import { createApp } from 'vue'
import { createPinia } from 'pinia'

import '@xterm/xterm/css/xterm.css'
import './styles.css'

import App from './App.vue'
import { router } from './router'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')
