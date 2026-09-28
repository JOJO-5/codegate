/**
 * ULID 生成器（Crockford Base32，26 字符）。
 *
 * 协议要求 request_id 用 ULID 或 UUIDv7（§9.2），两者的共同点是**时间有序** ——
 * 服务端日志按 id 排序就等于按时间排序，排障时不用再去关联时间戳字段。
 *
 * 为什么不用 `crypto.randomUUID()`：那是 v4，纯随机、完全无序，
 * 正好丢掉我们需要的那个性质。
 *
 * 为什么不为这 30 行引一个依赖：ULID 的规范就一页纸，
 * 而依赖会带来版本漂移和一个需要在 DLP 环境下额外解密的 node_modules 条目。
 *
 * 结构：48 位毫秒时间戳 + 80 位随机 = 128 位，编码成 26 个 Base32 字符。
 */

/** Crockford Base32 字母表：去掉了 I / L / O / U，避免与 1 / 0 混淆。 */
const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'

/** 时间戳部分占 10 个字符（48 位 / 5 ≈ 9.6，向上取整）。 */
const TIME_CHARS = 10

/** 随机部分占 16 个字符（80 位 / 5 = 16，正好整除，不需要处理余位）。 */
const RANDOM_CHARS = 16

/**
 * 生成一个 ULID。
 *
 * @param now 毫秒时间戳，默认取当前时间。允许注入是为了测试可复现。
 */
export function ulid(now: number = Date.now()): string {
  return encodeTime(now) + encodeRandom()
}

/**
 * 把毫秒时间戳编成 10 个字符。
 *
 * 用 `%32` 而不是位运算：JS 的位运算会把操作数截成 32 位有符号整数，
 * 而毫秒时间戳约 1.7e12，早就溢出了 —— 用 `>>>` 或 `|` 会得到负数。
 */
function encodeTime(now: number): string {
  let t = Math.floor(now)
  let out = ''
  for (let i = 0; i < TIME_CHARS; i++) {
    out = CROCKFORD[t % 32] + out
    t = Math.floor(t / 32)
  }
  return out
}

/**
 * 用 CSPRNG 生成 16 个随机字符（80 位）。
 *
 * 必须是密码学安全的随机源：request_id 会进日志、进 pending 注册表，
 * 可预测的 id 意味着可枚举 —— 那不是致命漏洞，但没有理由给它留口子。
 */
function encodeRandom(): string {
  const bytes = new Uint8Array(10) // 80 位
  crypto.getRandomValues(bytes)

  let out = ''
  for (let i = 0; i < RANDOM_CHARS; i++) {
    const start = i * 5
    const byteIdx = start >> 3
    const shift = start & 7
    const hi = bytes[byteIdx]
    // 最后一组会跨到不存在的第 11 个字节上，补 0 即可（多出的位会被掩码切掉）
    const lo = byteIdx + 1 < bytes.length ? bytes[byteIdx + 1] : 0
    const v = (((hi << 8) | lo) >> (11 - shift)) & 0x1f
    out += CROCKFORD[v]
  }
  return out
}
