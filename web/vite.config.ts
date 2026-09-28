import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时前端跑在 5173，后端默认在 8080。
//
// 为什么必须配代理而不是让前端直连后端（比如 http://127.0.0.1:8080/api）：
//
//   1. refresh token 是 HttpOnly Cookie，Path 固定为 /api/v1/auth。
//      如果页面来自 5173 而请求打到 8080，那就是跨站请求 —— Cookie 会被
//      浏览器丢掉（SameSite），刷新令牌直接失效，表现为「用着用着就被踢出去」。
//      走同源代理之后，浏览器眼里一切都在 5173，Cookie 正常携带。
//
//   2. /api/v1/ws/client 是 WebSocket，代理必须显式开 ws: true，
//      否则升级请求会被当成普通 HTTP 转发，握手直接失败。
//
// 生产环境不需要这些 —— 前端产物被 go:embed 进 Server，本来就是同源。
const BACKEND = 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    // xterm + 各 addon 加起来会超过 vite 默认 500KB 的警告线。
    // 这是终端模拟器的固有体积，不是代码问题，所以把线抬上去而不是拆包
    // —— 拆包只会让首屏多几次往返，对自托管工具是负优化。
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': { target: BACKEND, changeOrigin: true, ws: true },
    },
  },
})
