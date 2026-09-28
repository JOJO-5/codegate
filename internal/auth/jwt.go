package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	// ErrTokenInvalid 表示 access token 结构/签名/签发方不合法。
	ErrTokenInvalid = errors.New("auth: access token 无效")
	// ErrTokenExpired 单独一个错误，因为前端的处理不同：
	// 过期 → 静默刷新；无效 → 踢回登录页。
	ErrTokenExpired = errors.New("auth: access token 已过期")
)

// minSecretLen 是 HMAC 密钥的最短长度（256 bit）。
// HS256 的安全性上限就是密钥长度，短于此等于自降强度。
const minSecretLen = 32

// AccessClaims 是 access token 的声明集合。
//
// ★ 只放 sub / exp / iat / jti —— **不放任何权限声明**（§10.1）。
//
// 理由：权限写进 token 就会引入「吊销延迟」。用户被禁用、被改权限之后，
// 他手里那张还有 14 分钟有效期的 token 依然能通过校验。
// 权限每次查库的代价是一次主键查询，换来的是**即时吊销**能力 —— 值得。
type AccessClaims struct {
	jwt.RegisteredClaims
}

// TokenIssuer 签发与校验 access token。
//
// 无状态，可安全并发使用。
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	issuer string
}

// NewTokenIssuer 构造签发器。
//
// secret 长度在这里就卡住，而不是等到签发时 —— 一个 4 字节的 secret
// 能让程序跑得很好，直到有人把它爆破掉。
func NewTokenIssuer(secret []byte, ttl time.Duration) (*TokenIssuer, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("auth: JWT secret 至少 %d 字节，实际 %d", minSecretLen, len(secret))
	}
	if ttl <= 0 {
		return nil, errors.New("auth: access token TTL 必须为正")
	}
	return &TokenIssuer{secret: secret, ttl: ttl, issuer: "codegate"}, nil
}

// Issue 签发一个 access token。
//
// 同时返回 jti 与过期时刻：jti 用于把「这次登录」与后续日志关联起来
// （不用于吊销 —— access token 刻意设计成不可吊销，靠短 TTL 兜底）。
func (t *TokenIssuer) Issue(userID string, now time.Time) (token, jti string, expiresAt time.Time, err error) {
	if userID == "" {
		return "", "", time.Time{}, errors.New("auth: 签发 token 时 userID 为空")
	}

	jti = uuid.NewString()
	exp := now.Add(t.ttl)

	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    t.issuer,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("auth: 签发 access token 失败: %w", err)
	}
	return signed, jti, exp, nil
}

// Verify 校验 token 并返回声明。
func (t *TokenIssuer) Verify(raw string) (*AccessClaims, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: token 为空", ErrTokenInvalid)
	}

	var claims AccessClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(tok *jwt.Token) (any, error) {
			// ★ 必须显式校验算法。
			//
			// 不校验的话，攻击者可以把 header 改成 `alg: none` 或
			// `alg: RS256` 并把公钥当 HMAC 密钥用，从而伪造出
			// 「签名正确」的 token。这是 JWT 历史上最经典的一类漏洞。
			if tok.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, fmt.Errorf("%w: 签名算法 %q 不受支持",
					ErrTokenInvalid, tok.Method.Alg())
			}
			return t.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	// 库只保证 claims 结构合法，语义校验要自己做：
	// 一个没有 sub 的 token 通过了签名校验也是没用的。
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: 缺少 sub", ErrTokenInvalid)
	}
	return &claims, nil
}

// TTL 返回配置的有效期。用于在响应里回给前端 expires_in。
func (t *TokenIssuer) TTL() time.Duration { return t.ttl }
