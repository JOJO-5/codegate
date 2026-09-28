package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
)

// crockfordAlphabet 是 Crockford Base32 字母表。
//
// 相比标准 Base32（A-Z2-7）去掉了四个字符：
//   - I、L：与数字 1 视觉混淆
//   - O：与数字 0 视觉混淆
//   - U：Crockford 的设计考虑 —— 避免随机组合拼出冒犯性单词
//
// 这个选择的实际价值：用户会**手抄**这个码（从 Agent 终端念到手机上），
// 视觉歧义会直接变成「配对失败」的客服问题。
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// PairingCodeLen 是归一化后的字符数。
	PairingCodeLen = 8
	// pairingCodeBytes 是原始熵字节数。
	//
	// 5 字节 = 40 bit，恰好是 8 × 5 bit。40 bit 的空间意味着
	// 即使不做限流，随机猜中的概率也是 1/2^40 ≈ 9×10^-13；
	// 再叠加 10 分钟 TTL 与限流，暴力猜解不成立（§10.4）。
	pairingCodeBytes = 5
	// pairingGroupSize 是展示时的分组长度（`7F2K-93LM`）。
	pairingGroupSize = 4
)

// NewPairingCode 生成一个配对码。
//
// 返回明文（形如 `7F2K-93LM`，用于展示给用户）与存储哈希。
func NewPairingCode() (plain string, hash []byte, err error) {
	raw := make([]byte, pairingCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: 生成配对码失败: %w", err)
	}
	code := encodeCrockford(raw)
	return formatPairingCode(code), HashPairingCode(code), nil
}

// HashPairingCode 计算配对码的存储哈希。
//
// ★ 先归一化再哈希：用户输入的可能是 `7f2k 93lm`，而存储时用的是
// 归一化后的 `7F2K93LM`。不归一化就会出现「明明输入对了却说不匹配」。
//
// 用 sha256 而非慢哈希：配对码虽然只有 40 bit 熵（比刷新令牌弱得多），
// 但它的有效窗口只有 10 分钟且单次使用，且 code 空间 2^40 已经
// 让「拿着哈希离线枚举」的成本高到没有意义 —— 攻击者真正会做的是
// 在线猜解，那条路被限流堵住了（§10.4）。
func HashPairingCode(plain string) []byte {
	sum := sha256.Sum256([]byte(NormalizePairingCode(plain)))
	return sum[:]
}

// NormalizePairingCode 把用户输入归一化成标准形式。
//
// 做三件事：
//  1. 去掉分隔符（`-`、空格、制表符）—— 用户可能连着或不连着输入
//  2. 转大写
//  3. 按 Crockford 的规则做**歧义折叠**：I/L → 1，O → 0
//
// 第 3 条是关键：用户看到 `7F2K-93LM` 里的 `1`，可能输入字母 `l` 或 `I`；
// 看到 `0` 可能输入字母 `O`。这些输入必须被接受，否则配对的失败率
// 会高到无法使用。
func NormalizePairingCode(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range strings.ToUpper(strings.TrimSpace(in)) {
		switch r {
		case '-', ' ', '\t', '_':
			continue
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidPairingCodeFormat 判断输入归一化后是否是合法的配对码格式。
//
// 这个函数的作用是**快速失败**：格式明显不对的输入不必去查库，
// 直接拒绝能省掉一次 DB 往返，也少一个限流的计数来源。
func ValidPairingCodeFormat(in string) bool {
	norm := NormalizePairingCode(in)
	if len(norm) != PairingCodeLen {
		return false
	}
	for i := 0; i < len(norm); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(norm[i])) {
			return false
		}
	}
	return true
}

// encodeCrockford 把 5 字节编码成 8 个 Base32 字符。
//
// 按大端序读取 40 位，再从高位到低位每 5 位取一个字符。
func encodeCrockford(raw []byte) string {
	var v uint64
	for _, b := range raw {
		v = v<<8 | uint64(b)
	}

	out := make([]byte, PairingCodeLen)
	for i := PairingCodeLen - 1; i >= 0; i-- {
		out[i] = crockfordAlphabet[v&0x1F]
		v >>= 5
	}
	return string(out)
}

// formatPairingCode 插入分组连字符，便于用户抄写与朗读。
func formatPairingCode(code string) string {
	if len(code) <= pairingGroupSize {
		return code
	}
	var parts []string
	for i := 0; i < len(code); i += pairingGroupSize {
		end := min(i+pairingGroupSize, len(code))
		parts = append(parts, code[i:end])
	}
	return strings.Join(parts, "-")
}
