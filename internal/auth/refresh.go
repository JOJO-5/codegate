package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// RefreshTokenBytes 是刷新令牌的原始熵长度。
//
// 32 字节 = 256 bit，远超任何可行的暴力枚举范围。
const RefreshTokenBytes = 32

// NewRefreshToken 生成一个不透明刷新令牌。
//
// 返回明文与存储哈希。明文只出现一次（回给用户），DB 里只存哈希。
//
// ★ 为什么是「不透明随机串」而不是 JWT：刷新令牌必须能被**即时吊销**，
// 而 JWT 是自包含的、无状态的 —— 想吊销就得维护黑名单，那还不如直接用
// 随机串 + 查库。这里选的是可吊销性。
func NewRefreshToken() (plain string, hash []byte, err error) {
	buf := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: 生成刷新令牌失败: %w", err)
	}
	// URL-safe 无 padding：这个值会出现在 cookie 里，不能含 `+` `/` `=`。
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashRefreshToken(plain), nil
}

// HashRefreshToken 计算刷新令牌的存储哈希。
//
// ★ 用 sha256 而不是 argon2id，这是刻意的：
//
// 慢哈希（argon2/bcrypt）存在的意义是抵抗**弱输入**的暴力枚举 ——
// 人类密码只有几十 bit 熵，所以要让每次尝试都很贵。
// 而刷新令牌是 256 bit 的均匀随机值，枚举它在计算上不可行，
// 慢哈希带来的唯一效果就是每次刷新多花 50ms。
//
// 所以这里选快哈希。安全性由熵保证，不由计算成本保证。
func HashRefreshToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}
