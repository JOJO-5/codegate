/**
 * 终端尺寸估算。
 *
 * # 为什么需要它
 *
 * `session.create` 必须带 `cols` / `rows` —— PTY 从创建那一刻就需要尺寸，
 * 不能等到浏览器 attach 之后再设。而创建会话的动作发生在设备详情页，
 * 那时还没有 xterm 实例可以问。
 *
 * 所以这里按视口粗算一个初始值。它**不需要准**：
 * 终端页打开后 FitAddon 会算出真实尺寸并立即发 `session.resize`，
 * ConPTY 会重新布局。估错的唯一后果是 TUI 在头一两百毫秒里排版不对。
 *
 * 但也不能差太远：如果估成 80×24 而实际是 40×60（手机横屏），
 * Claude Code 这类全屏 TUI 会先按宽屏画一遍，然后被 resize 打断重画，
 * 视觉上就是"闪一下"。所以按真实视口算，别用固定值。
 */

/** 等宽字体的近似字符宽度（CSS px）。xterm 默认字号是 15px，Monaco/Menlo 大约 0.6em。 */
const CHAR_WIDTH_PX = 9
const LINE_HEIGHT_PX = 17

/** 上下界：太小会让 TUI 排版崩掉，太大则超出 ConPTY 能接受的范围。 */
const MIN_COLS = 20
const MAX_COLS = 500
const MIN_ROWS = 5
const MAX_ROWS = 300

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, v))
}

/** 按当前视口估算一个终端尺寸。 */
export function estimateTerminalSize(): { cols: number; rows: number } {
  const w = typeof window === 'undefined' ? 1024 : window.innerWidth
  const h = typeof window === 'undefined' ? 768 : window.innerHeight

  // 减掉终端页的顶栏 / 底栏与内边距，避免一开始就算多一行一列
  const usableW = Math.max(0, w - 24)
  const usableH = Math.max(0, h - 90)

  return {
    cols: clamp(Math.floor(usableW / CHAR_WIDTH_PX), MIN_COLS, MAX_COLS),
    rows: clamp(Math.floor(usableH / LINE_HEIGHT_PX), MIN_ROWS, MAX_ROWS),
  }
}
