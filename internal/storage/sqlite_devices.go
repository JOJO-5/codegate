package storage

import (
	"context"
	"database/sql"
	"time"
)

// ---------------------------------------------------------------------------
// devices
// ---------------------------------------------------------------------------

// deviceCols 的 user_id / last_seen_at / paired_at / revoked_at 都是可空的，
// 扫描时统一用 sql.Null* 接，再转成 Go 的零值或指针。
const deviceCols = `id, user_id, name, platform, arch, agent_version, public_key,
	last_seen_at, paired_at, revoked_at, created_at`

func (s *SQLite) CreateDevice(ctx context.Context, d *Device) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices
		 (id, user_id, name, platform, arch, agent_version, public_key,
		  last_seen_at, paired_at, revoked_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, nullIfEmpty(d.UserID), d.Name, d.Platform, d.Arch, d.AgentVersion,
		d.PublicKey, toMsArg(d.LastSeenAt), toMsArg(d.PairedAt), toMsArg(d.RevokedAt),
		toMs(d.CreatedAt),
	)
	return wrapErr(err)
}

func (s *SQLite) DeviceByID(ctx context.Context, id string) (*Device, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deviceCols+` FROM devices WHERE id = ?`, id)
	return scanDevice(row)
}

// DevicesByUser 列出某账号的全部设备。
//
// 只返回已绑定的（user_id 非空）。未绑定设备对任何账号都不可见 ——
// 它们只存在于 pairing 流程里，通过 code 关联。
func (s *SQLite) DevicesByUser(ctx context.Context, userID string) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deviceCols+` FROM devices WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()

	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, wrapErr(rows.Err())
}

// RenameDevice 改设备显示名。
//
// 不带 user_id 条件 —— 鉴权是上层的责任（authz.go），
// 存储层不重复实现授权。这看起来危险，但把授权逻辑复制到每个 SQL 里
// 才是真正危险的：总有一处会漏。
func (s *SQLite) RenameDevice(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return wrapErr(err)
	}
	return requireAffected(res, "设备")
}

// BindDevice 把设备绑定到用户。
//
// 条件 `user_id IS NULL OR user_id = ?` 保证两件事：
//  1. 一台设备不会被两个账号同时绑走（防抢绑）
//  2. 重复绑定到同一账号是幂等的，不会误报冲突
func (s *SQLite) BindDevice(ctx context.Context, id, userID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET user_id = ?, paired_at = ?, revoked_at = NULL
		 WHERE id = ? AND (user_id IS NULL OR user_id = ?)`,
		userID, toMs(at), id, userID)
	if err != nil {
		return wrapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr(err)
	}
	if n == 0 {
		// 0 行有两种可能，必须区分 —— 上层要给出不同的错误提示。
		if _, lookupErr := s.DeviceByID(ctx, id); lookupErr != nil {
			return lookupErr // ErrNotFound
		}
		return ErrConflict // 已被别的账号绑定
	}
	return nil
}

// DeleteDevice 删除设备记录。其 sessions 由外键 ON DELETE CASCADE 一并清除。
func (s *SQLite) DeleteDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return wrapErr(err)
	}
	return requireAffected(res, "设备")
}

// TouchDeviceLastSeen 更新最后在线时间。
//
// 不检查受影响行数：Agent 可能在配对完成前就断开，此时设备记录
// 可能已被删除，更新 0 行是正常情况，不该报错。
func (s *SQLite) TouchDeviceLastSeen(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`, toMs(at), id)
	return wrapErr(err)
}

func scanDevice(sc rowScanner) (*Device, error) {
	var (
		d         Device
		userID    sql.NullString
		lastSeen  sql.NullInt64
		pairedAt  sql.NullInt64
		revokedAt sql.NullInt64
		createdAt int64
	)
	err := sc.Scan(&d.ID, &userID, &d.Name, &d.Platform, &d.Arch, &d.AgentVersion,
		&d.PublicKey, &lastSeen, &pairedAt, &revokedAt, &createdAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	d.UserID = userID.String
	d.LastSeenAt = msPtr(lastSeen)
	d.PairedAt = msPtr(pairedAt)
	d.RevokedAt = msPtr(revokedAt)
	d.CreatedAt = fromMs(createdAt)
	return &d, nil
}

// ---------------------------------------------------------------------------
// pairing_codes
// ---------------------------------------------------------------------------

const pairingCodeCols = `id, code_hash, device_id, public_key, name, platform, arch,
	agent_version, agent_ip, expires_at, used_at, used_by, created_at`

func (s *SQLite) CreatePairingCode(ctx context.Context, p *PairingCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pairing_codes
		 (id, code_hash, device_id, public_key, name, platform, arch,
		  agent_version, agent_ip, expires_at, used_at, used_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.CodeHash, p.DeviceID, p.PublicKey, p.Name, p.Platform, p.Arch,
		p.AgentVersion, p.AgentIP, toMs(p.ExpiresAt), toMsArg(p.UsedAt),
		nullIfEmpty(p.UsedBy), toMs(p.CreatedAt),
	)
	return wrapErr(err)
}

// PairingCodeByHash 按 code 的哈希查找。
//
// ★ 存哈希不存明文（§10.4）：DB 泄露时攻击者拿到哈希也无法
// 在 10 分钟有效期内提交它（提交的是明文，服务端再哈希比对）。
func (s *SQLite) PairingCodeByHash(ctx context.Context, hash []byte) (*PairingCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pairingCodeCols+` FROM pairing_codes WHERE code_hash = ?`, hash)
	return scanPairingCode(row)
}

// MarkPairingCodeUsed 原子地把配对码标记为已使用。
//
// `used_at IS NULL` 条件让并发的两个 confirm 请求只有一个能成功 ——
// 这是「单次使用」这条防护的落地点（§10.4）。
func (s *SQLite) MarkPairingCodeUsed(ctx context.Context, id, userID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairing_codes SET used_at = ?, used_by = ?
		 WHERE id = ? AND used_at IS NULL`,
		toMs(at), userID, id)
	if err != nil {
		return wrapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr(err)
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

func (s *SQLite) DeleteExpiredPairingCodes(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pairing_codes WHERE expires_at < ?`, toMs(before))
	if err != nil {
		return 0, wrapErr(err)
	}
	return res.RowsAffected()
}

func scanPairingCode(sc rowScanner) (*PairingCode, error) {
	var (
		p         PairingCode
		agentIP   sql.NullString
		expiresAt int64
		usedAt    sql.NullInt64
		usedBy    sql.NullString
		createdAt int64
	)
	err := sc.Scan(&p.ID, &p.CodeHash, &p.DeviceID, &p.PublicKey, &p.Name,
		&p.Platform, &p.Arch, &p.AgentVersion, &agentIP, &expiresAt,
		&usedAt, &usedBy, &createdAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	p.AgentIP = agentIP.String
	p.ExpiresAt = fromMs(expiresAt)
	p.UsedAt = msPtr(usedAt)
	p.UsedBy = usedBy.String
	p.CreatedAt = fromMs(createdAt)
	return &p, nil
}

// nullIfEmpty 把空串转成 SQL NULL。
//
// ★ 这不是美化，是正确性要求：devices.user_id 为空串时外键约束会失败
// （没有 id 为 "" 的用户），而 NULL 才表示「未绑定」。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
