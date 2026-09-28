package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestStore 打开一个临时文件数据库并完成迁移。
//
// ★ 刻意**不用内存库**（`:memory:`）。内存库有两条与真实部署不同的行为：
//   - 每次连接都是**独立的空库**（连接池里第二条连接看不到第一条建的表）
//   - WAL 模式在内存库上是 no-op，测不到真实的并发行为
//
// 用临时文件才能测到真正要测的东西（R9 的锁行为就在这里）。
func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	ctx := context.Background()

	s, err := OpenSQLite(ctx, SQLiteOptions{
		Path:   filepath.Join(t.TempDir(), "test.db"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return s
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("生成 UUIDv7 失败: %v", err)
	}
	return id.String()
}

func seedUser(t *testing.T, s *SQLite, email string) *User {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	u := &User{
		ID:           newID(t),
		Email:        email,
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$fake",
		Role:         "user",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return u
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 第二次迁移必须是无操作而不是报错 —— Server 每次启动都会调它。
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移应当幂等，实际报错: %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("读取迁移记录失败: %v", err)
	}
	if n != 1 {
		t.Errorf("迁移记录应当只有 1 条，实际 %d 条（说明被重复执行了）", n)
	}
}

func TestPragmasApplied(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// WAL 是 R9 的前提条件，必须确认真的生效 ——
	// 内存库上它会静默失效，所以这个断言只有在文件库上才有意义。
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode 应为 wal，实际 %q", mode)
	}

	// 外键在 SQLite 里默认关闭，我们显式打开了它。
	// 这条一旦失效，删除设备时 sessions 不会被级联清理，会留下孤儿行。
	var fk int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("读取 foreign_keys 失败: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys 应为 1，实际 %d", fk)
	}
}

func TestUserEmailIsCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "JOJO@Example.COM")

	// 同一个邮箱的不同大小写必须命中同一条记录，
	// 否则会出现「用 JoJo@example.com 登录，却提示用户不存在」。
	got, err := s.UserByEmail(ctx, "jojo@example.com")
	if err != nil {
		t.Fatalf("小写查询失败: %v", err)
	}
	if got.Email != "JOJO@Example.COM" {
		t.Errorf("拿到的邮箱不对: %q", got.Email)
	}

	// 反向也要成立。
	if _, err := s.UserByEmail(ctx, "JOJO@EXAMPLE.COM"); err != nil {
		t.Fatalf("大写查询失败: %v", err)
	}

	// 重复注册（换个大小写）必须被唯一索引挡住。
	err = s.CreateUser(ctx, &User{
		ID: newID(t), Email: "jojo@example.com", PasswordHash: "x",
		Role: "user", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("大小写不同的重复邮箱应当冲突，实际: %v", err)
	}
}

func TestUserByIDNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UserByID(context.Background(), newID(t))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("查不存在的用户应返回 ErrNotFound，实际: %v", err)
	}
}

func TestCountUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Errorf("初始应为 0，实际 %d", n)
	}

	seedUser(t, s, "a@x.com")
	seedUser(t, s, "b@x.com")

	if n, _ = s.CountUsers(ctx); n != 2 {
		t.Errorf("应为 2，实际 %d", n)
	}
}

func TestUpdateUserPassword(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.UpdateUserPassword(ctx, u.ID, "$argon2id$new", now); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}

	got, _ := s.UserByID(ctx, u.ID)
	if got.PasswordHash != "$argon2id$new" {
		t.Errorf("密码哈希没更新: %q", got.PasswordHash)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at 应为 %v，实际 %v", now, got.UpdatedAt)
	}

	// 不存在的用户 → ErrNotFound（而不是静默成功）。
	err := s.UpdateUserPassword(ctx, newID(t), "x", now)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在的用户应返回 ErrNotFound，实际: %v", err)
	}
}

// TestRefreshTokenRotationAndReuseDetection 覆盖 §10.1 的核心安全机制。
func TestRefreshTokenRotationAndReuseDetection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")

	now := time.Now().UTC().Truncate(time.Millisecond)
	family := newID(t)
	rt := &RefreshToken{
		ID:        newID(t),
		UserID:    u.ID,
		TokenHash: []byte("hash-1"),
		FamilyID:  family,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
		CreatedAt: now,
	}
	if err := s.CreateRefreshToken(ctx, rt); err != nil {
		t.Fatalf("创建刷新令牌失败: %v", err)
	}

	// 正常轮换：标记已用成功。
	if err := s.MarkRefreshTokenUsed(ctx, rt.ID, now); err != nil {
		t.Fatalf("首次标记已用应当成功: %v", err)
	}

	// ★ 重用检测：同一个令牌再标记一次必须失败。
	// 这是「攻击者拿到旧令牌」的信号，上层据此吊销整族。
	err := s.MarkRefreshTokenUsed(ctx, rt.ID, now)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重复使用同一令牌应返回 ErrConflict，实际: %v", err)
	}

	// 查出来的记录应当已是「已用」状态。
	got, err := s.RefreshTokenByHash(ctx, []byte("hash-1"))
	if err != nil {
		t.Fatalf("按哈希查询失败: %v", err)
	}
	if got.Valid(now) {
		t.Error("已轮换的令牌不应再被视为有效")
	}
	if got.UsedAt == nil {
		t.Error("used_at 应当已写入")
	}

	// 整族吊销。
	n, err := s.RevokeRefreshTokenFamily(ctx, family, now)
	if err != nil {
		t.Fatalf("吊销整族失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应当吊销 1 条，实际 %d", n)
	}

	got, _ = s.RefreshTokenByHash(ctx, []byte("hash-1"))
	if got.RevokedAt == nil {
		t.Error("revoked_at 应当已写入")
	}
}

func TestRefreshTokenByHashNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.RefreshTokenByHash(context.Background(), []byte("nope"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，实际: %v", err)
	}
}

func TestRevokeUserRefreshTokensAndCleanup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i, exp := range []time.Time{
		now.Add(24 * time.Hour),
		now.Add(-time.Hour), // 已过期
	} {
		err := s.CreateRefreshToken(ctx, &RefreshToken{
			ID: newID(t), UserID: u.ID,
			TokenHash: []byte{byte(i)}, FamilyID: newID(t),
			ExpiresAt: exp, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("创建令牌 %d 失败: %v", i, err)
		}
	}

	n, err := s.RevokeUserRefreshTokens(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("吊销全部失败: %v", err)
	}
	if n != 2 {
		t.Errorf("应当吊销 2 条，实际 %d", n)
	}

	// 清理只删过期行，不碰吊销行。
	deleted, err := s.DeleteExpiredRefreshTokens(ctx, now)
	if err != nil {
		t.Fatalf("清理过期失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("应当清理 1 条过期令牌，实际 %d", deleted)
	}
}
