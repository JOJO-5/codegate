package device

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/storage"
)

// fakeOnline 是一个可控的在线状态表。
type fakeOnline map[string]bool

func (f fakeOnline) IsOnline(id string) bool { return f[id] }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestService 起一套真实的 SQLite + Service。
//
// ★ 不手写 fake store：Store 接口有 30 多个方法，写一个只会
// 「恰好满足当前测试」的假实现，等于把测试和实现绑死 ——
// 实现改了接口，假实现跟着改，但**真正的 SQL 一行都没被验证过**。
// 用真库跑真 SQL 才有意义（R9 的锁行为也只能这样测）。
func newTestService(t *testing.T) (*Service, storage.Store, fakeOnline) {
	t.Helper()
	ctx := context.Background()

	store, err := storage.OpenSQLite(ctx, storage.SQLiteOptions{
		Path:   filepath.Join(t.TempDir(), "device-test.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	online := fakeOnline{}
	return NewService(store, online, quietLogger()), store, online
}

func seedUser(t *testing.T, store storage.Store, email string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := mustUUID(t)
	if err := store.CreateUser(context.Background(), &storage.User{
		ID: id, Email: email, PasswordHash: "$argon2id$fake",
		Role: "user", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return id
}

func seedDevice(t *testing.T, store storage.Store, userID, name string) string {
	t.Helper()
	id := mustUUID(t)
	if err := store.CreateDevice(context.Background(), &storage.Device{
		ID: id, UserID: userID, Name: name,
		Platform: "windows", Arch: "amd64", AgentVersion: "0.1.0",
		PublicKey: []byte("0123456789abcdef0123456789abcdef"),
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	return id
}

func mustUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("生成 UUID 失败: %v", err)
	}
	return id.String()
}

func TestListOnlyReturnsOwnDevices(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	bob := seedUser(t, store, "bob@x.com")

	seedDevice(t, store, alice, "Alice-1")
	seedDevice(t, store, alice, "Alice-2")
	seedDevice(t, store, bob, "Bob-1")
	seedDevice(t, store, "", "未绑定设备")

	list, err := svc.List(ctx, alice)
	if err != nil {
		t.Fatalf("列出设备失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Alice 应当看到 2 台设备，实际 %d", len(list))
	}
	for _, v := range list {
		if v.UserID != alice {
			t.Errorf("列表里混入了别人的设备: %s", v.ID)
		}
	}
}

// TestGetRejectsOtherUsersDevice 是本包最重要的测试（防 IDOR）。
func TestGetRejectsOtherUsersDevice(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	bob := seedUser(t, store, "bob@x.com")
	aliceDevice := seedDevice(t, store, alice, "Alice-PC")

	// Bob 拿着 Alice 的 device_id 去查 → 必须拿不到。
	if _, err := svc.Get(ctx, bob, aliceDevice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("★ 跨账号读取设备必须失败，实际: %v", err)
	}

	// ★ 错误必须与「设备压根不存在」完全一致，否则可以拿来枚举有效 ID。
	_, errMissing := svc.Get(ctx, bob, mustUUID(t))
	_, errForeign := svc.Get(ctx, bob, aliceDevice)
	if errMissing.Error() != errForeign.Error() {
		t.Errorf("「不存在」与「无权访问」的错误信息必须相同，实际:\n  不存在: %v\n  无权:   %v",
			errMissing, errForeign)
	}

	// Alice 自己查得到。
	if _, err := svc.Get(ctx, alice, aliceDevice); err != nil {
		t.Errorf("本人读取应当成功，实际: %v", err)
	}
}

func TestRenameRejectsOtherUsersDevice(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	bob := seedUser(t, store, "bob@x.com")
	aliceDevice := seedDevice(t, store, alice, "Alice-PC")

	if _, err := svc.Rename(ctx, bob, aliceDevice, "被改名了"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("★ 跨账号改名必须失败，实际: %v", err)
	}

	// 确认名字没被改动。
	got, err := svc.Get(ctx, alice, aliceDevice)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Alice-PC" {
		t.Errorf("★ 设备名被越权修改了: %q", got.Name)
	}
}

func TestDeleteRejectsOtherUsersDevice(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	bob := seedUser(t, store, "bob@x.com")
	aliceDevice := seedDevice(t, store, alice, "Alice-PC")

	if err := svc.Delete(ctx, bob, aliceDevice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("★ 跨账号删除必须失败，实际: %v", err)
	}

	// 设备必须还在。
	if _, err := svc.Get(ctx, alice, aliceDevice); err != nil {
		t.Errorf("★ 设备被越权删除了: %v", err)
	}

	// 本人删得掉。
	if err := svc.Delete(ctx, alice, aliceDevice); err != nil {
		t.Fatalf("本人删除应当成功: %v", err)
	}
	if _, err := svc.Get(ctx, alice, aliceDevice); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应当查不到: %v", err)
	}
}

func TestRenameValidation(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()
	alice := seedUser(t, store, "alice@x.com")
	dev := seedDevice(t, store, alice, "原名")

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"空串", "", ErrEmptyName},
		{"全空白", "   \t ", ErrEmptyName},
		{"超长", string(make([]rune, MaxNameLen+1)), ErrNameTooLong},
		{"含控制字符", "bad\x00name", nil}, // 会被单独的错误拒绝
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Rename(ctx, alice, dev, tc.in)
			if err == nil {
				t.Fatalf("非法名字 %q 应当被拒绝", tc.in)
			}
		})
	}

	// 合法名字（含中文）应当通过，且两端空白被 trim。
	v, err := svc.Rename(ctx, alice, dev, "  书房台式机  ")
	if err != nil {
		t.Fatalf("合法名字应当通过: %v", err)
	}
	if v.Name != "书房台式机" {
		t.Errorf("应当去掉两端空白，实际: %q", v.Name)
	}
}

// TestNameLengthCountsRunes 确认长度按字符算而不是按字节。
//
// 如果按字节算，「书房台式机」是 15 字节，64 字符的中文名字会被
// 误判为超长 —— 这是中文用户的直接体验问题。
func TestNameLengthCountsRunes(t *testing.T) {
	// 64 个中文字符 = 192 字节，应当合法。
	name := ""
	for i := 0; i < MaxNameLen; i++ {
		name += "书"
	}
	if err := ValidateName(name); err != nil {
		t.Errorf("64 个中文字符应当合法（按字符计），实际: %v", err)
	}
}

func TestOnlineStatusReflectsChecker(t *testing.T) {
	svc, store, online := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	dev := seedDevice(t, store, alice, "PC")

	// 默认离线。
	list, _ := svc.List(ctx, alice)
	if len(list) != 1 || list[0].Online {
		t.Error("未标记在线的设备应报告离线")
	}

	online[dev] = true
	list, _ = svc.List(ctx, alice)
	if !list[0].Online {
		t.Error("标记在线后应报告在线")
	}

	v, _ := svc.Get(ctx, alice, dev)
	if !v.Online {
		t.Error("Get 也应带上实时在线状态")
	}
}

func TestBindAndConflict(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	alice := seedUser(t, store, "alice@x.com")
	bob := seedUser(t, store, "bob@x.com")
	// 未绑定设备（pairing 待确认状态）。
	dev := seedDevice(t, store, "", "待配对设备")

	if err := svc.Bind(ctx, alice, dev); err != nil {
		t.Fatalf("首次绑定应当成功: %v", err)
	}

	// 重复绑定到同一账号应当幂等。
	if err := svc.Bind(ctx, alice, dev); err != nil {
		t.Fatalf("重复绑定应当幂等: %v", err)
	}

	// 换账号绑定 → 必须失败，且错误是领域错误而不是 storage 的哨兵。
	err := svc.Bind(ctx, bob, dev)
	if !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("★ 抢绑应当返回 ErrAlreadyPaired，实际: %v", err)
	}

	// 绑定不存在的设备 → ErrNotFound。
	if err := svc.Bind(ctx, bob, mustUUID(t)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("绑定不存在的设备应返回 ErrNotFound，实际: %v", err)
	}
}

func TestMarkSeen(t *testing.T) {
	svc, store, _ := newTestService(t)
	ctx := context.Background()

	fixed := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })

	alice := seedUser(t, store, "alice@x.com")
	dev := seedDevice(t, store, alice, "PC")

	if err := svc.MarkSeen(ctx, dev); err != nil {
		t.Fatalf("更新在线时间失败: %v", err)
	}

	v, _ := svc.Get(ctx, alice, dev)
	if v.LastSeenAt == nil || !v.LastSeenAt.Equal(fixed) {
		t.Errorf("last_seen_at 应为 %v，实际 %v", fixed, v.LastSeenAt)
	}
}
