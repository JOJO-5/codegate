// Package auth 实现用户认证原语：密码哈希、access token、refresh token、配对码。
//
// 这一层**只做密码学与格式**，不碰数据库、不发 HTTP。
// 需要存储的编排逻辑（轮换、重用检测、审计）在上层（internal/server）完成。
// 这样切分的好处：密码学部分可以被穷举测试，而不用搭一套 DB。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id 参数（§10.1）。
//
// 这些数字不是拍脑袋的：Argon2id 的推荐基线是「至少 19 MiB 内存」，
// 我们给到 64 MiB + 3 轮迭代，在现代 CPU 上单次约 50-100ms ——
// 对登录接口完全可接受，但对离线爆破是实打实的成本。
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB = 64 MiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
)

// 密码长度限制。
const (
	// MinPasswordLen 是允许的最短密码。
	MinPasswordLen = 8
	// MaxPasswordLen 存在的理由不是「密码太长记不住」，而是防资源消耗：
	// 攻击者可以提交 10 MB 的「密码」让我们去哈希，这是白送的 DoS 面。
	MaxPasswordLen = 1024
)

var (
	// ErrPasswordTooShort / ErrPasswordTooLong 是密码长度校验错误。
	ErrPasswordTooShort = errors.New("auth: 密码太短")
	ErrPasswordTooLong  = errors.New("auth: 密码太长")
	// ErrInvalidHash 表示数据库里的哈希串格式非法（数据损坏或被人改过）。
	ErrInvalidHash = errors.New("auth: 密码哈希格式非法")
	// ErrPasswordMismatch 表示密码不匹配。
	ErrPasswordMismatch = errors.New("auth: 密码不匹配")
)

// ValidatePassword 检查密码是否满足长度要求。
//
// ★ 刻意**不做**「必须含大小写+数字+符号」这类复杂度规则：
// NIST SP 800-63B 已明确建议不要强加组合规则（会诱导用户用 `Passw0rd!`），
// 长度才是有效维度。
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("%w: 至少 %d 个字符", ErrPasswordTooShort, MinPasswordLen)
	}
	if len(password) > MaxPasswordLen {
		return fmt.Errorf("%w: 最多 %d 个字符", ErrPasswordTooLong, MaxPasswordLen)
	}
	return nil
}

// HashPassword 生成 argon2id 哈希，返回自描述的编码串。
//
// 编码格式（与 argon2 参考实现一致）：
//
//	$argon2id$v=19$m=65536,t=3,p=4$<b64(salt)>$<b64(key)>
//
// 参数写进哈希串本身，是为了将来调参时**旧密码仍可验证** ——
// 验证时按串里的参数重算，而不是按当前常量。
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐失败: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// 用 RawStdEncoding（无 padding）—— 与参考实现一致，
	// 便于将来与其他语言的实现互操作。
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key),
	), nil
}

// VerifyPassword 校验密码。
//
// 返回 (true, nil) 表示匹配；(false, nil) 表示密码不对；
// error 只在**哈希串本身损坏**时返回 —— 调用方需要区分这两种情况：
// 前者记 login.fail，后者说明数据有问题，要记 error 级别的审计。
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt,
		params.time, params.memory, params.threads, uint32(len(want)))

	// ★ 必须用常数时间比较。用 bytes.Equal 会在第一个不同字节处返回，
	// 攻击者可以通过测量响应时间逐字节猜出哈希 —— 虽然需要大量样本，
	// 但没有理由为省一行代码而留下这个侧信道。
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash 判断一个已有哈希是否需要用当前参数重新计算。
//
// 用途：登录成功后顺手检查，参数过时就把明文密码重新哈希一遍存回去。
// 这是「可以悄悄升级安全参数」的常见做法，不需要强制用户改密码。
func NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		// 解析不了的哈希必须重算 —— 它已经不可用了。
		return true
	}
	return params.memory != argonMemory ||
		params.time != argonTime ||
		params.threads != argonThreads
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams

	parts := strings.Split(encoded, "$")
	// 形如 ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<key>"]
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, fmt.Errorf("%w: 字段数不对", ErrInvalidHash)
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: 算法 %q 不受支持", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("%w: 版本字段非法", ErrInvalidHash)
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: 版本 %d 不受支持", ErrInvalidHash, version)
	}

	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &threads); err != nil {
		return p, nil, nil, fmt.Errorf("%w: 参数字段非法", ErrInvalidHash)
	}
	// 参数来自数据库，可能是被篡改或损坏的值。不加限制的话，
	// 一个 `m=4294967295` 就能让验证时申请 4 TB 内存 → 直接 OOM。
	if p.memory == 0 || p.memory > 1024*1024 || p.time == 0 || p.time > 100 ||
		threads == 0 || threads > 255 {
		return p, nil, nil, fmt.Errorf("%w: 参数超出合理范围", ErrInvalidHash)
	}
	p.threads = uint8(threads)

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, fmt.Errorf("%w: 盐解码失败", ErrInvalidHash)
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return p, nil, nil, fmt.Errorf("%w: 哈希解码失败", ErrInvalidHash)
	}
	return p, salt, key, nil
}
