package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const pw = "correct horse battery staple"

	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}

	ok, err := VerifyPassword(encoded, pw)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if !ok {
		t.Error("正确密码应当验证通过")
	}

	ok, err = VerifyPassword(encoded, pw+"x")
	if err != nil {
		t.Fatalf("验证错误密码不应报错，实际: %v", err)
	}
	if ok {
		t.Error("★ 错误密码必须验证失败")
	}
}

// TestHashIsSalted 确认同样的密码两次哈希结果不同。
//
// 如果没加盐（或盐固定），攻击者可以用一张彩虹表同时破解所有
// 使用相同密码的账号 —— 而且能从哈希相等直接看出「这两个用户密码一样」。
func TestHashIsSalted(t *testing.T) {
	a, err := HashPassword("same-password-123")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("★ 相同密码的两次哈希不应相同（盐没生效）")
	}

	// 但两个都必须能验证通过。
	for i, enc := range []string{a, b} {
		ok, err := VerifyPassword(enc, "same-password-123")
		if err != nil || !ok {
			t.Errorf("第 %d 个哈希验证失败: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestEncodedFormat(t *testing.T) {
	encoded, err := HashPassword("some-password-1")
	if err != nil {
		t.Fatal(err)
	}

	// 自描述格式：参数写在串里，将来调参时旧密码仍可验证。
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("哈希串前缀不符合预期: %q", encoded)
	}
	if n := len(strings.Split(encoded, "$")); n != 6 {
		t.Errorf("哈希串应有 6 个字段（含首个空串），实际 %d: %q", n, encoded)
	}
	// 明文密码绝不能出现在哈希串里。
	if strings.Contains(encoded, "some-password-1") {
		t.Error("★ 哈希串里出现了明文密码")
	}
}

func TestPasswordLengthValidation(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		want error
	}{
		{"太短", "abc", ErrPasswordTooShort},
		{"刚好最短", strings.Repeat("a", MinPasswordLen), nil},
		{"太长", strings.Repeat("a", MaxPasswordLen+1), ErrPasswordTooLong},
		{"刚好最长", strings.Repeat("a", MaxPasswordLen), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw)
			if tc.want == nil {
				if err != nil {
					t.Errorf("应当通过，实际: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("应当返回 %v，实际: %v", tc.want, err)
			}
		})
	}
}

func TestHashPasswordRejectsShort(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("应当拒绝过短密码，实际: %v", err)
	}
}

func TestVerifyRejectsCorruptedHash(t *testing.T) {
	// 数据库里的哈希串可能因为各种原因损坏。必须明确报 ErrInvalidHash，
	// 而不是 panic 或误判为「密码正确」。
	corrupted := []string{
		"",
		"not-a-hash",
		"$argon2id$v=19$m=65536,t=3,p=4$onlyfivefields",
		"$bcrypt$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",   // 算法不对
		"$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$aGFzaA", // 版本不对
		"$argon2id$v=19$m=abc,t=3,p=4$c2FsdA$aGFzaA",   // 参数非数字
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",    // 盐不是合法 base64
	}
	for _, enc := range corrupted {
		ok, err := VerifyPassword(enc, "anything")
		if ok {
			t.Errorf("★ 损坏的哈希 %q 被判定为验证通过", enc)
		}
		if !errors.Is(err, ErrInvalidHash) {
			t.Errorf("哈希 %q 应返回 ErrInvalidHash，实际: %v", enc, err)
		}
	}
}

// TestVerifyRejectsInsaneParams 覆盖一个真实的 DoS 面：
// 哈希串里的 m 参数来自数据库，如果不加范围检查，
// 一个被篡改成 m=4294967295 的记录会让验证时申请 4 TB 内存。
func TestVerifyRejectsInsaneParams(t *testing.T) {
	insane := []string{
		"$argon2id$v=19$m=4294967295,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=99999,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=0,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	}
	for _, enc := range insane {
		_, err := VerifyPassword(enc, "pw")
		if !errors.Is(err, ErrInvalidHash) {
			t.Errorf("参数越界的哈希 %q 应被拒绝，实际: %v", enc, err)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	current, err := HashPassword("password-123")
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(current) {
		t.Error("用当前参数生成的哈希不应需要重算")
	}

	// 参数比当前弱的旧哈希 → 需要重算。
	weaker := "$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNo"
	if !NeedsRehash(weaker) {
		t.Error("参数过时的哈希应当需要重算")
	}

	// 解析不了的哈希必须重算（它已经不可用了）。
	if !NeedsRehash("garbage") {
		t.Error("无法解析的哈希应当需要重算")
	}
}

func TestVerifyIsConstantTime(t *testing.T) {
	// 这个测试不能真正测出时序差异（那需要统计大量样本），
	// 但它确保代码走的是 ConstantTimeCompare 那条路 ——
	// 如果哪天有人改成 `string(got) == string(want)`，至少这里的
	// 语义断言（相同长度、不同内容必须返回 false）还在。
	encoded, err := HashPassword("password-123")
	if err != nil {
		t.Fatal(err)
	}

	// 长度相同但内容不同。
	ok, _ := VerifyPassword(encoded, "password-124")
	if ok {
		t.Error("长度相同但内容不同的密码不应通过")
	}
}
