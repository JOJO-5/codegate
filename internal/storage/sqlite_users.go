package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的共同能力，让扫描函数只写一份。
type rowScanner interface {
	Scan(dest ...any) error
}

// boolToInt 把布尔转成 SQLite 的 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

const userCols = `id, email, password_hash, role, disabled, created_at, updated_at`

func (s *SQLite) CreateUser(ctx context.Context, u *User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, role, disabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, boolToInt(u.Disabled),
		toMs(u.CreatedAt), toMs(u.UpdatedAt),
	)
	return wrapErr(err)
}

// UserByEmail 按邮箱查用户。
//
// 用 `lower(email)` 匹配而不是直接等值：DB 里建的是 `lower(email)` 唯一索引，
// 等值查询会走不到索引（SQLite 不做隐式大小写折叠），并且
// `JOJO@x.com` 与 `jojo@x.com` 必须视为同一个账号，否则会出现重复注册。
func (s *SQLite) UserByEmail(ctx context.Context, email string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE lower(email) = lower(?)`, email)
	return scanUser(row)
}

func (s *SQLite) UserByID(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// CountUsers 返回用户总数。用于「一个用户都没有时允许注册第一个」的引导逻辑。
func (s *SQLite) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, wrapErr(err)
}

// UpdateUserPassword 改密码。同时更新 updated_at，便于审计判断凭证何时变更。
func (s *SQLite) UpdateUserPassword(ctx context.Context, id, hash string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, toMs(now), id)
	if err != nil {
		return wrapErr(err)
	}
	return requireAffected(res, "用户")
}

// ListUsers 列出全部账号。
//
// 按 created_at 升序（而不是降序）：管理命令的典型用途是
// 「看看这个实例上有哪些账号」，而账号列表通常很短，
// 按创建顺序读最符合直觉 —— 第一个是引导账号。
//
// ★ 不返回 password_hash 以外的敏感字段 —— User 结构里本来也只有它，
// 而调用方（管理命令）不该把它打出来。这里不做脱敏是因为
// 「存储层返回完整记录」和「展示层决定显示什么」是两件事。
func (s *SQLite) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM users ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, wrapErr(rows.Err())
}

// SetUserDisabled 停用/启用账号。
//
// ★ 只改 disabled，不动 password_hash 与 role。
//
// 一个很容易犯的错是「停用 = 把密码改成一个随机值」。那样做有两个问题：
//  1. 不可逆 —— 解禁之后用户必须走一次「忘记密码」流程，
//     而管理员只是想暂时关掉它。
//  2. 会写坏审计 —— updated_at 变了，事后无法区分
//     「密码被改过」和「只是被停用过」。
func (s *SQLite) SetUserDisabled(ctx context.Context, id string, disabled bool, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(disabled), toMs(now), id)
	if err != nil {
		return wrapErr(err)
	}
	return requireAffected(res, "用户")
}

func scanUser(sc rowScanner) (*User, error) {
	var (
		u        User
		disabled int
		created  int64
		updated  int64
	)
	err := sc.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &disabled, &created, &updated)
	if err != nil {
		return nil, wrapErr(err)
	}
	u.Disabled = disabled != 0
	u.CreatedAt = fromMs(created)
	u.UpdatedAt = fromMs(updated)
	return &u, nil
}

// ---------------------------------------------------------------------------
// refresh_tokens
// ---------------------------------------------------------------------------

const refreshTokenCols = `id, user_id, token_hash, family_id, expires_at,
	used_at, revoked_at, user_agent, ip, created_at`

func (s *SQLite) CreateRefreshToken(ctx context.Context, t *RefreshToken) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens
		 (id, user_id, token_hash, family_id, expires_at, used_at, revoked_at, user_agent, ip, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.TokenHash, t.FamilyID, toMs(t.ExpiresAt),
		toMsArg(t.UsedAt), toMsArg(t.RevokedAt), t.UserAgent, t.IP, toMs(t.CreatedAt),
	)
	return wrapErr(err)
}

// RefreshTokenByHash 按令牌哈希查找。查不到返回 ErrNotFound。
//
// ★ 注意：查到了不代表可用 —— 调用方必须再调 Valid(now) 判断
// 是否已过期 / 已轮换 / 已吊销。把「存在」和「可用」分开是有意的：
// 已轮换的令牌被再次使用是**攻击信号**，需要能拿到它才能触发整族吊销。
func (s *SQLite) RefreshTokenByHash(ctx context.Context, hash []byte) (*RefreshToken, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+refreshTokenCols+` FROM refresh_tokens WHERE token_hash = ?`, hash)
	return scanRefreshToken(row)
}

// MarkRefreshTokenUsed 标记令牌已被轮换。
//
// WHERE 里带 `used_at IS NULL` 让它成为一次**原子的比较并交换**：
// 两个并发刷新请求同时到达时，只有一个能把 NULL 改成时间戳，
// 另一个拿到 0 行受影响 → 返回 ErrConflict。这就是防重放的关键。
func (s *SQLite) MarkRefreshTokenUsed(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		toMs(at), id)
	if err != nil {
		return wrapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr(err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 刷新令牌 %s 已被使用过", ErrConflict, id)
	}
	return nil
}

// RevokeRefreshTokenFamily 吊销整族令牌。
//
// 检测到「已用过的令牌被重放」时必须这样做（§10.1）：
// 这说明令牌可能已被窃取，攻击者与合法用户都在用它。
// 只吊销单个令牌没有意义 —— 攻击者手里还有轮换后的新令牌。
func (s *SQLite) RevokeRefreshTokenFamily(ctx context.Context, familyID string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = ?
		 WHERE family_id = ? AND revoked_at IS NULL`,
		toMs(at), familyID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return res.RowsAffected()
}

// RevokeUserRefreshTokens 吊销某用户全部刷新令牌（改密码 / 登出所有设备）。
func (s *SQLite) RevokeUserRefreshTokens(ctx context.Context, userID string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = ?
		 WHERE user_id = ? AND revoked_at IS NULL`,
		toMs(at), userID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return res.RowsAffected()
}

// DeleteExpiredRefreshTokens 清理过期记录，由后台定时任务调用。
//
// 不做「顺手在查询时删」：删除是写操作，塞进读路径会让读也去抢写锁。
func (s *SQLite) DeleteExpiredRefreshTokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM refresh_tokens WHERE expires_at < ?`, toMs(before))
	if err != nil {
		return 0, wrapErr(err)
	}
	return res.RowsAffected()
}

func scanRefreshToken(sc rowScanner) (*RefreshToken, error) {
	var (
		t         RefreshToken
		expiresAt int64
		usedAt    sql.NullInt64
		revokedAt sql.NullInt64
		userAgent sql.NullString
		ip        sql.NullString
		createdAt int64
	)
	err := sc.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.FamilyID, &expiresAt,
		&usedAt, &revokedAt, &userAgent, &ip, &createdAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	t.ExpiresAt = fromMs(expiresAt)
	t.UsedAt = msPtr(usedAt)
	t.RevokedAt = msPtr(revokedAt)
	t.UserAgent = userAgent.String
	t.IP = ip.String
	t.CreatedAt = fromMs(createdAt)
	return &t, nil
}

// requireAffected 在 UPDATE/DELETE 影响到 0 行时返回 ErrNotFound。
//
// 用于「更新一个必须存在的对象」这类场景（改密码、重命名设备）。
// 对幂等操作（标记已用、吊销）不要用它 —— 那些地方 0 行是合法结果。
func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr(err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s不存在", ErrNotFound, what)
	}
	return nil
}
