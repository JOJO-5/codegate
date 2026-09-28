package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// access token
// ---------------------------------------------------------------------------

func newTestIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	iss, err := NewTokenIssuer([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	return iss
}

func TestNewTokenIssuerRejectsShortSecret(t *testing.T) {
	// 短密钥等于自降 HMAC 强度。必须在构造时就拒绝，而不是等出事。
	if _, err := NewTokenIssuer([]byte("short"), time.Minute); err == nil {
		t.Error("★ 过短的 secret 必须被拒绝")
	}
	if _, err := NewTokenIssuer([]byte(strings.Repeat("k", 32)), 0); err == nil {
		t.Error("TTL 为 0 必须被拒绝")
	}
}

func TestIssueAndVerify(t *testing.T) {
	iss := newTestIssuer(t)
	now := time.Now()

	token, jti, exp, err := iss.Issue("user-123", now)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if token == "" || jti == "" {
		t.Fatal("token 与 jti 都不应为空")
	}
	if got := exp.Sub(now); got != 15*time.Minute {
		t.Errorf("过期时间应为 15 分钟，实际 %v", got)
	}

	claims, err := iss.Verify(token)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("sub 应为 user-123，实际 %q", claims.Subject)
	}
	if claims.ID != jti {
		t.Errorf("jti 应为 %q，实际 %q", jti, claims.ID)
	}
}

func TestIssueRejectsEmptyUserID(t *testing.T) {
	if _, _, _, err := newTestIssuer(t).Issue("", time.Now()); err == nil {
		t.Error("空 userID 必须被拒绝")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	iss := newTestIssuer(t)
	// 用一个很久以前的时间签发 → 现在必然已过期。
	token, _, _, err := iss.Issue("user-1", time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	_, err = iss.Verify(token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("过期 token 应返回 ErrTokenExpired，实际: %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	iss := newTestIssuer(t)
	token, _, _, err := iss.Issue("user-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	other, err := NewTokenIssuer([]byte(strings.Repeat("x", 32)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("用别的密钥验证应当失败，实际: %v", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	iss := newTestIssuer(t)
	token, _, _, err := iss.Issue("user-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应当有 3 段，实际 %d", len(parts))
	}

	// 把 sub 改成别人，签名不变 → 必须被拒绝。
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	claims["sub"] = "victim"
	tampered, _ := json.Marshal(claims)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]

	if _, err := iss.Verify(forged); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("★ 被篡改的 token 必须验证失败，实际: %v", err)
	}
}

// TestVerifyRejectsAlgNone 覆盖 JWT 最经典的一类漏洞。
//
// 攻击者把 header 的 alg 改成 `none` 并去掉签名。如果服务端不显式校验算法，
// 某些库会「按照 token 自己声明的算法」去验，于是直接放行。
func TestVerifyRejectsAlgNone(t *testing.T) {
	iss := newTestIssuer(t)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"sub":"attacker","iss":"codegate","exp":%d}`, time.Now().Add(time.Hour).Unix())))
	forged := header + "." + payload + "."

	if _, err := iss.Verify(forged); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("★ alg=none 的伪造 token 必须被拒绝，实际: %v", err)
	}

	// 连签名段都不给（两段形式）也必须拒绝。
	if _, err := iss.Verify(header + "." + payload); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("★ 无签名的 token 必须被拒绝，实际: %v", err)
	}
}

func TestVerifyRejectsWrongIssuer(t *testing.T) {
	iss := newTestIssuer(t)
	// 换一个 issuer 签发的 token，即使密钥相同也不该接受。
	other, err := NewTokenIssuer([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other.issuer = "someone-else"

	token, _, _, err := other.Issue("user-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Verify(token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("issuer 不匹配应当被拒绝，实际: %v", err)
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	if _, err := newTestIssuer(t).Verify(""); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("空 token 应当被拒绝，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// refresh token
// ---------------------------------------------------------------------------

func TestNewRefreshToken(t *testing.T) {
	seen := make(map[string]bool)

	for i := 0; i < 20; i++ {
		plain, hash, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if seen[plain] {
			t.Fatal("★ 刷新令牌出现重复 —— 随机源有问题")
		}
		seen[plain] = true

		// 32 字节 → base64url 无 padding 是 43 个字符。
		if len(plain) != 43 {
			t.Errorf("明文长度应为 43，实际 %d: %q", len(plain), plain)
		}
		if len(hash) != 32 {
			t.Errorf("sha256 应为 32 字节，实际 %d", len(hash))
		}
		// URL-safe：不能出现 `+` `/` `=`，否则放进 cookie 会出问题。
		if strings.ContainsAny(plain, "+/=") {
			t.Errorf("令牌含非 URL-safe 字符: %q", plain)
		}
	}
}

func TestHashRefreshTokenIsDeterministic(t *testing.T) {
	plain, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}

	again := HashRefreshToken(plain)
	if string(again) != string(hash) {
		t.Error("同一输入的哈希必须一致（否则刷新时查不到记录）")
	}
	if string(HashRefreshToken(plain+"x")) == string(hash) {
		t.Error("不同输入不应产生相同哈希")
	}
}

// ---------------------------------------------------------------------------
// pairing code
// ---------------------------------------------------------------------------

func TestNewPairingCode(t *testing.T) {
	seen := make(map[string]bool)

	for i := 0; i < 50; i++ {
		plain, hash, err := NewPairingCode()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if seen[plain] {
			t.Fatal("配对码重复")
		}
		seen[plain] = true

		// 展示格式：8 位 + 中间一个连字符。
		if len(plain) != PairingCodeLen+1 {
			t.Errorf("展示长度应为 %d，实际 %d: %q", PairingCodeLen+1, len(plain), plain)
		}
		if plain[4] != '-' {
			t.Errorf("连字符位置不对: %q", plain)
		}
		if len(hash) != 32 {
			t.Errorf("哈希应为 32 字节，实际 %d", len(hash))
		}

		// ★ 字符集里绝不能出现 I / L / O / U —— 这是 Crockford 的核心价值。
		norm := NormalizePairingCode(plain)
		for _, bad := range []string{"I", "L", "O", "U"} {
			if strings.Contains(norm, bad) {
				t.Errorf("★ 配对码出现了易混淆字符 %q: %q", bad, plain)
			}
		}
	}
}

func TestNormalizePairingCode(t *testing.T) {
	// 用例里的码刻意用 M/Q（都在 Crockford 字母表内），避免与下面的
	// 歧义折叠用例混在一起看不出重点。
	cases := map[string]string{
		"7F2K-93MQ":   "7F2K93MQ", // 标准展示形式
		"7f2k-93mq":   "7F2K93MQ", // 小写
		"7F2K 93MQ":   "7F2K93MQ", // 空格分隔
		"7F2K93MQ":    "7F2K93MQ", // 无分隔
		" 7F2K-93MQ ": "7F2K93MQ", // 首尾空白
		"7F2K_93MQ":   "7F2K93MQ", // 下划线分隔
		"ABCDEF":      "ABCDEF",   // 全在字母表内，原样

		// ★ 歧义折叠 —— 这是归一化真正的价值所在。
		// 字母表里没有 I / L / O / U，用户手抄时看到 `1` 可能写成 `I` 或 `l`，
		// 看到 `0` 可能写成 `O`。这些输入必须被接受，否则配对的失败率
		// 会高到没法用。
		"OI1L": "0111", // O→0, I→1, 1→1, L→1
		"o-1l": "011",
	}
	for in, want := range cases {
		if got := NormalizePairingCode(in); got != want {
			t.Errorf("NormalizePairingCode(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestPairingCodeHashIsNormalized 确认「用户怎么输入都能匹配上」。
func TestPairingCodeHashIsNormalized(t *testing.T) {
	plain, hash, err := NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}

	// 用各种书写方式重新计算哈希，都必须等于存储时的哈希。
	variants := []string{
		plain,
		strings.ToLower(plain),
		strings.ReplaceAll(plain, "-", ""),
		strings.ReplaceAll(plain, "-", " "),
		"  " + plain + "  ",
	}
	for _, v := range variants {
		if string(HashPairingCode(v)) != string(hash) {
			t.Errorf("输入变体 %q 未能匹配到同一哈希", v)
		}
	}
}

func TestValidPairingCodeFormat(t *testing.T) {
	valid := []string{"7F2K-93LM", "7f2k93lm", "0000-0000", "ZZZZ-ZZZZ"}
	for _, v := range valid {
		if !ValidPairingCodeFormat(v) {
			t.Errorf("%q 应当是合法格式", v)
		}
	}

	invalid := []string{
		"",           // 空
		"7F2K",       // 太短
		"7F2K-93LMX", // 太长
		"7F2K-93L!",  // 非法字符
		"7F2K-93L中",  // 非 ASCII
		"I234-5678X", // X 合法，但整体 9 位
	}
	for _, v := range invalid {
		if ValidPairingCodeFormat(v) {
			t.Errorf("%q 不应是合法格式", v)
		}
	}

	// I/L/O 会被折叠成 1/1/0，所以含它们的输入在长度正确时**是合法的** ——
	// 这正是设计意图（用户抄错了也要能配对）。
	if !ValidPairingCodeFormat("I234-5678") {
		t.Error("含 I 的输入应当合法（会被折叠成 1）")
	}
}
