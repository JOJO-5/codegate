/**
 * 路由表（架构文档 §5.2）。
 *
 * ```
 * /login           登录
 * /                重定向到 /devices
 * /devices         设备列表
 * /devices/:id     设备详情 + 会话列表 + 新建会话
 * /sessions/:id    终端页（xterm.js 全屏）
 * /settings        改密码、审计日志
 * ```
 *
 * # 为什么除登录页外全部懒加载
 *
 * 终端页拖着 xterm.js 和四个 addon，是整个前端最大的一块。
 * 设备列表页在手机上打开时完全不需要它 —— 静态 import 会把它塞进首屏包，
 * 让"打开设备列表"变成"下载一个终端模拟器"。按路由切分是免费的。
 */

import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useAuthStore } from './stores/auth'

const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/projects' },
  { path: '/projects', name: 'projects', component: () => import('./views/ProjectsView.vue'), meta: { title: '项目' } },

  {
    path: '/login',
    name: 'login',
    component: () => import('./views/LoginView.vue'),
    meta: { public: true, chrome: false },
  },
  {
    path: '/devices',
    name: 'devices',
    component: () => import('./views/DevicesView.vue'),
    meta: { title: '设备' },
  },
  {
    path: '/devices/:id',
    name: 'device',
    component: () => import('./views/DeviceDetailView.vue'),
    props: true,
    meta: { title: '设备详情' },
  },
  {
    path: '/sessions/:id',
    name: 'session',
    component: () => import('./views/SessionView.vue'),
    props: true,
    // 终端页是全屏的：不要外壳，否则在手机上会白白吃掉一条导航栏的高度。
    meta: { chrome: false },
  },
  {
    path: '/sessions/:id/files',
    name: 'session-files',
    component: () => import('./views/FilesView.vue'),
    props: true,
    meta: { title: '工作区文件' },
  },
  {
    path: '/sessions/:id/git',
    name: 'session-git',
    component: () => import('./views/GitView.vue'),
    props: true,
    meta: { title: 'Git 改动' },
  },
  {
    path: '/settings',
    name: 'settings',
    component: () => import('./views/SettingsView.vue'),
    meta: { title: '设置' },
  },

  { path: '/:pathMatch(.*)*', redirect: '/devices' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

/**
 * 认证守卫。
 *
 * ★ 这里 await 冷启动而不是"先放行、失败再踢"：后者会让用户先看到
 *   一帧空白的设备列表，再被弹回登录页 —— 那个闪烁很廉价地暴露了
 *   "这个应用没搞清楚自己有没有登录"。
 *
 * `bootstrap()` 是幂等的（内部共享 Promise），所以每次导航都调它
 * 不会产生并发的 refresh 请求 —— 那会触发服务端的 refresh 重用检测，
 * 导致整族吊销、用户被莫名踢下线。
 */
router.beforeEach(async (to) => {
  const auth = useAuthStore()
  await auth.bootstrap()

  if (to.meta['public'] === true) {
    // 已登录还去登录页 → 直接送去设备列表
    return auth.isLoggedIn ? { name: 'projects' } : true
  }

  if (!auth.isLoggedIn) {
    return { name: 'login', query: { next: to.fullPath } }
  }
  return true
})
