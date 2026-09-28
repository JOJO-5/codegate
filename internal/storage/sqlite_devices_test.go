package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedDevice(t *testing.T, s *SQLite, userID string) *Device {
	t.Helper()
	d := &Device{
		ID:           newID(t),
		UserID:       userID,
		Name:         "JOJO-PC",
		Platform:     "windows",
		Arch:         "amd64",
		AgentVersion: "0.1.0",
		PublicKey:    []byte("0123456789abcdef0123456789abcdef"),
		CreatedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.CreateDevice(context.Background(), d); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	return d
}

// TestDeviceBindAndSteal 验证「一台设备不能被两个账号绑走」（§10.4 防抢绑）。
func TestDeviceBindAndSteal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	alice := seedUser(t, s, "alice@x.com")
	bob := seedUser(t, s, "bob@x.com")
	// 设备先以「未绑定」状态存在（对应 Agent 已 pair.begin 但还没确认）。
	dev := seedDevice(t, s, "")

	if dev.Paired() {
		t.Fatal("新设备不应是已绑定状态")
	}

	now := time.Now().UTC().Truncate(time.Millisecond)

	// Alice 绑定成功。
	if err := s.BindDevice(ctx, dev.ID, alice.ID, now); err != nil {
		t.Fatalf("Alice 绑定应当成功: %v", err)
	}

	// 重复绑定到同一账号应当幂等成功（刷新页面后重新确认是常见操作）。
	if err := s.BindDevice(ctx, dev.ID, alice.ID, now); err != nil {
		t.Fatalf("重复绑定同一账号应当幂等: %v", err)
	}

	// ★ Bob 试图绑走 Alice 的设备 → 必须冲突。
	if err := s.BindDevice(ctx, dev.ID, bob.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("抢绑应当返回 ErrConflict，实际: %v", err)
	}

	// 确认设备仍属于 Alice。
	got, _ := s.DeviceByID(ctx, dev.ID)
	if got.UserID != alice.ID {
		t.Errorf("设备归属被改动了: %q", got.UserID)
	}
	if got.PairedAt == nil {
		t.Error("paired_at 应当已写入")
	}

	// 绑定不存在的设备 → ErrNotFound（而不是 ErrConflict）。
	if err := s.BindDevice(ctx, newID(t), alice.ID, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("绑定不存在的设备应返回 ErrNotFound，实际: %v", err)
	}
}

func TestDevicesByUserIsolatesAccounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	alice := seedUser(t, s, "alice@x.com")
	bob := seedUser(t, s, "bob@x.com")

	seedDevice(t, s, alice.ID)
	seedDevice(t, s, alice.ID)
	seedDevice(t, s, bob.ID)
	seedDevice(t, s, "") // 未绑定，任何人都不该看到

	list, err := s.DevicesByUser(ctx, alice.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("Alice 应有 2 台设备，实际 %d", len(list))
	}

	list, _ = s.DevicesByUser(ctx, bob.ID)
	if len(list) != 1 {
		t.Errorf("Bob 应有 1 台设备，实际 %d", len(list))
	}
}

func TestRenameDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dev := seedDevice(t, s, "")

	if err := s.RenameDevice(ctx, dev.ID, "书房台式机"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	got, _ := s.DeviceByID(ctx, dev.ID)
	if got.Name != "书房台式机" {
		t.Errorf("名字没更新: %q", got.Name)
	}

	if err := s.RenameDevice(ctx, newID(t), "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在的设备应返回 ErrNotFound，实际: %v", err)
	}
}

func TestTouchDeviceLastSeen(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dev := seedDevice(t, s, "")

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.TouchDeviceLastSeen(ctx, dev.ID, now); err != nil {
		t.Fatalf("更新在线时间失败: %v", err)
	}
	got, _ := s.DeviceByID(ctx, dev.ID)
	if got.LastSeenAt == nil || !got.LastSeenAt.Equal(now) {
		t.Errorf("last_seen_at 不正确: %v", got.LastSeenAt)
	}

	// 对不存在的设备更新应当是**静默无操作**而不是报错 ——
	// Agent 可能在配对完成前就断开，那时设备记录还不存在。
	if err := s.TouchDeviceLastSeen(ctx, newID(t), now); err != nil {
		t.Errorf("更新不存在的设备不应报错，实际: %v", err)
	}
}

// TestDeleteDeviceCascadesSessions 验证外键级联真的生效。
//
// 这条直接依赖 PRAGMA foreign_keys=1 —— 它在 SQLite 里默认是关的，
// 一旦 DSN 配置漏了，这个测试会立刻变红。
func TestDeleteDeviceCascadesSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := seedUser(t, s, "a@x.com")
	dev := seedDevice(t, s, u.ID)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.UpsertSession(ctx, &SessionMeta{
		ID: newID(t), DeviceID: dev.ID, UserID: u.ID,
		Name: "Claude Code", Command: "claude.exe", Cwd: "C:/work",
		Status: "running", Cols: 100, Rows: 30, CreatedAt: now,
	}); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}

	if err := s.DeleteDevice(ctx, dev.ID); err != nil {
		t.Fatalf("删除设备失败: %v", err)
	}

	// 设备没了，会话必须跟着走。
	list, err := s.SessionsByDevice(ctx, dev.ID)
	if err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("设备删除后应当没有会话，实际残留 %d 条（外键级联失效？）", len(list))
	}

	// 重复删除 → ErrNotFound。
	if err := s.DeleteDevice(ctx, dev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound，实际: %v", err)
	}
}

// TestPairingCodeSingleUse 验证配对码的原子单次使用（§10.4）。
func TestPairingCodeSingleUse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")

	now := time.Now().UTC().Truncate(time.Millisecond)
	pc := &PairingCode{
		ID: newID(t), CodeHash: []byte("code-hash"),
		DeviceID: newID(t), PublicKey: []byte("pub"),
		Name: "JOJO-PC", Platform: "windows", Arch: "amd64",
		AgentVersion: "0.1.0", AgentIP: "203.0.113.7",
		ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now,
	}
	if err := s.CreatePairingCode(ctx, pc); err != nil {
		t.Fatalf("创建配对码失败: %v", err)
	}

	got, err := s.PairingCodeByHash(ctx, []byte("code-hash"))
	if err != nil {
		t.Fatalf("查询配对码失败: %v", err)
	}
	if !got.Usable(now) {
		t.Error("新配对码应当可用")
	}
	if got.AgentIP != "203.0.113.7" {
		t.Errorf("agent_ip 应当保留（用户确认界面要用）: %q", got.AgentIP)
	}

	if err := s.MarkPairingCodeUsed(ctx, pc.ID, u.ID, now); err != nil {
		t.Fatalf("首次标记已用应当成功: %v", err)
	}

	// ★ 第二次必须失败 —— 这是防重放。
	if err := s.MarkPairingCodeUsed(ctx, pc.ID, u.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复使用配对码应返回 ErrConflict，实际: %v", err)
	}

	got, _ = s.PairingCodeByHash(ctx, []byte("code-hash"))
	if got.Usable(now) {
		t.Error("已使用的配对码不应再可用")
	}
	if got.UsedBy != u.ID {
		t.Errorf("used_by 应为 %q，实际 %q", u.ID, got.UsedBy)
	}
}

func TestPairingCodeExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	pc := &PairingCode{
		ID: newID(t), CodeHash: []byte("old-code"), DeviceID: newID(t),
		PublicKey: []byte("pub"), Name: "x", Platform: "linux", Arch: "arm64",
		AgentVersion: "0.1.0", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-11 * time.Minute),
	}
	if err := s.CreatePairingCode(ctx, pc); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	got, _ := s.PairingCodeByHash(ctx, []byte("old-code"))
	if got.Usable(now) {
		t.Error("已过期的配对码不应可用")
	}

	n, err := s.DeleteExpiredPairingCodes(ctx, now)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应当清理 1 条，实际 %d", n)
	}
}

func TestPairingCodeByHashNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.PairingCodeByHash(context.Background(), []byte("nope"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，实际: %v", err)
	}
}

// TestSessionUpsertAndPrune 覆盖 Agent 重连对账的核心路径（§7.4）。
func TestSessionUpsertAndPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")
	dev := seedDevice(t, s, u.ID)

	now := time.Now().UTC().Truncate(time.Millisecond)
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = newID(t)
		err := s.UpsertSession(ctx, &SessionMeta{
			ID: ids[i], DeviceID: dev.ID, UserID: u.ID,
			Name: "sess", Command: "cmd.exe", Args: []string{"/c", "dir"},
			Cwd: "C:/work", Status: "running", PID: 100 + i,
			Cols: 80, Rows: 24, CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("写入会话 %d 失败: %v", i, err)
		}
	}

	// Upsert 同一 ID 应当更新而不是插入新行（携带完整元数据）。
	err := s.UpsertSession(ctx, &SessionMeta{
		ID: ids[0], DeviceID: dev.ID, UserID: u.ID,
		Name: "改过名的会话", Command: "cmd.exe", Args: []string{"/c", "dir"},
		Cwd: "C:/work", Status: "running", Cols: 80, Rows: 24, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Upsert 更新失败: %v", err)
	}

	list, _ := s.SessionsByDevice(ctx, dev.ID)
	if len(list) != 3 {
		t.Fatalf("应当有 3 条会话，实际 %d（Upsert 变成了插入？）", len(list))
	}
	// 最新创建的排在最前。
	if list[0].ID != ids[2] {
		t.Errorf("排序应为 created_at DESC，实际首条是 %s", list[0].ID)
	}

	// 心跳路径：只更新运行态。
	//
	// ★ 这里如果误用 UpsertSession，args/name/command/cwd 会被空值覆盖 ——
	// 这正是 UpdateSessionRuntime 存在的理由。
	err = s.UpdateSessionRuntime(ctx, ids[0], SessionRuntime{
		Status: "exited", Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatalf("更新运行态失败: %v", err)
	}

	got, _ := s.SessionByID(ctx, ids[0])
	if got.Status != "exited" {
		t.Errorf("状态没被更新: %q", got.Status)
	}
	if len(got.Args) != 2 || got.Args[1] != "dir" {
		t.Errorf("心跳更新不应清空 args，实际: %v", got.Args)
	}
	if got.Name != "改过名的会话" {
		t.Errorf("心跳更新不应改动 name，实际: %q", got.Name)
	}

	// 对账：Agent 说「我只剩 ids[0] 和 ids[1]」，ids[2] 应当被清掉。
	n, err := s.PruneSessions(ctx, dev.ID, []string{ids[0], ids[1]})
	if err != nil {
		t.Fatalf("对账清理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应当清理 1 条残留会话，实际 %d", n)
	}

	list, _ = s.SessionsByDevice(ctx, dev.ID)
	if len(list) != 2 {
		t.Errorf("清理后应当剩 2 条，实际 %d", len(list))
	}

	// keep 为空 = Agent 现在没有任何会话 → 全清。
	if _, err := s.PruneSessions(ctx, dev.ID, nil); err != nil {
		t.Fatalf("全量清理失败: %v", err)
	}
	if list, _ = s.SessionsByDevice(ctx, dev.ID); len(list) != 0 {
		t.Errorf("应当全部清空，实际剩 %d 条", len(list))
	}
}

func TestSessionByIDNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.SessionByID(context.Background(), newID(t))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，实际: %v", err)
	}
}

func TestAuditInsertAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := seedUser(t, s, "a@x.com")

	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, action := range []string{"login.ok", "device.pair", "session.create"} {
		err := s.InsertAudit(ctx, &AuditLog{
			ID: newID(t), UserID: u.ID, Action: action, Result: "ok",
			IP: "203.0.113.7", UserAgent: "Mozilla/5.0",
			MetaJSON:  `{"k":"v"}`,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("写审计 %d 失败: %v", i, err)
		}
	}

	list, err := s.AuditLogsByUser(ctx, u.ID, 10)
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应当有 3 条，实际 %d", len(list))
	}
	// 时间倒序：最后写的排最前。
	if list[0].Action != "session.create" {
		t.Errorf("排序应为 created_at DESC，实际首条 %q", list[0].Action)
	}
	if list[0].MetaJSON != `{"k":"v"}` {
		t.Errorf("meta_json 往返丢失: %q", list[0].MetaJSON)
	}
}
