package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// sessions（Server 侧元数据缓存，非真相源）
// ---------------------------------------------------------------------------

const sessionCols = `id, device_id, user_id, name, command, args_json, cwd, status,
	pid, exit_code, cols, rows, created_at, started_at, ended_at, last_attached_at, created_by, archived`

// UpsertSession 写入或覆盖一条会话元数据。
//
// 用 UPSERT 而不是「先查再插/改」：Agent 的 session.created 与心跳对账
// 可能几乎同时到达，两步写法会产生竞态（两次查询之间状态已变）。
//
// ★ 刻意**不覆盖** created_at：它是「这个会话什么时候建的」，
// 由 Agent 首次上报确定，后续心跳不该改写它。
func (s *SQLite) UpsertSession(ctx context.Context, m *SessionMeta) error {
	argsJSON, err := marshalArgs(m.Args)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions
		 (id, device_id, user_id, name, command, args_json, cwd, status,
		  pid, exit_code, cols, rows, created_at, started_at, ended_at, last_attached_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name = excluded.name,
		   command = excluded.command,
		   args_json = excluded.args_json,
		   cwd = excluded.cwd,
		   status = excluded.status,
		   pid = excluded.pid,
		   exit_code = excluded.exit_code,
		   cols = excluded.cols,
		   rows = excluded.rows,
		   started_at = excluded.started_at,
		   ended_at = excluded.ended_at,
		   last_attached_at = excluded.last_attached_at`,
		m.ID, m.DeviceID, m.UserID, m.Name, m.Command, argsJSON, m.Cwd, m.Status,
		nullIfZero(m.PID), toIntArg(m.ExitCode), int(m.Cols), int(m.Rows),
		toMs(m.CreatedAt), toMsArg(m.StartedAt), toMsArg(m.EndedAt),
		toMsArg(m.LastAttachedAt), nullIfEmpty(m.CreatedBy),
	)
	return wrapErr(err)
}

// UpdateSessionRuntime 只更新运行态字段。
//
// ★ 这是心跳对账的专用写路径。不能复用 UpsertSession ——
// 心跳的 payload 里没有 name/command/args/cwd，走 Upsert 会把它们清空。
//
// 会话不存在时返回 nil：心跳与 Prune 可能并发，一条刚被清理的会话
// 紧接着收到心跳是正常竞态，不该记成错误刷日志。
func (s *SQLite) UpdateSessionRuntime(ctx context.Context, id string, rt SessionRuntime) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		 SET status = ?, cols = ?, rows = ?, pid = ?, exit_code = ?, ended_at = ?
		 WHERE id = ?`,
		rt.Status, int(rt.Cols), int(rt.Rows), nullIfZero(rt.PID),
		toIntArg(rt.ExitCode), toMsArg(rt.EndedAt), id)
	return wrapErr(err)
}

func (s *SQLite) SessionByID(ctx context.Context, id string) (*SessionMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// SessionsByDevice 列出某设备下的会话，最近创建的在前。
//
// 排序用 created_at DESC 而不是 last_attached_at：用户找的是
// 「我刚建的那个会话」，而不是「我最后碰过的」—— 后者会让
// 一条老会话因为一次误触跳到列表顶部。
func (s *SQLite) SessionsByDevice(ctx context.Context, deviceID string) ([]*SessionMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE device_id = ? ORDER BY created_at DESC`,
		deviceID)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	return collectSessions(rows)
}

// SessionsByUser 跨设备列出某账号的会话（设置页 / 总览用）。
func (s *SQLite) SessionsByUser(ctx context.Context, userID string, limit int) ([]*SessionMeta, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE user_id = ?
		 ORDER BY created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	return collectSessions(rows)
}

func (s *SQLite) SetSessionArchived(ctx context.Context, id string, archived bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET archived = ? WHERE id = ?`, archived, id)
	return wrapErr(err)
}

func (s *SQLite) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return wrapErr(err)
}

// PruneSessions 删除该设备下不在 keep 里的会话记录。
//
// 这是 Agent 重连对账的收尾动作（§7.4）：Agent 上报「我当前持有这些会话」，
// 其余记录说明是上一次进程生命周期留下的残留（Agent 重启必然丢 Session，R5）。
// 不清理的话设备页会永远显示一堆早就死掉的会话。
//
// keep 为空表示「Agent 现在一个会话都没有」，此时清空该设备全部记录。
func (s *SQLite) PruneSessions(ctx context.Context, deviceID string, keep []string) (int64, error) {
	if len(keep) == 0 {
		res, err := s.db.ExecContext(ctx,
			`UPDATE sessions SET status = 'exited', ended_at = COALESCE(ended_at, ?)
			 WHERE device_id = ? AND status IN ('starting','running','detached')`, nowMs(), deviceID)
		if err != nil {
			return 0, wrapErr(err)
		}
		return res.RowsAffected()
	}

	// 占位符个数受 Agent 的 max_sessions 限制（默认 20），
	// 远低于 SQLite 的变量上限，不需要分批。
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, 0, len(keep)+2)
	args = append(args, nowMs(), deviceID)
	for _, id := range keep {
		args = append(args, id)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET status = 'exited', ended_at = COALESCE(ended_at, ?)
		 WHERE device_id = ? AND id NOT IN (`+placeholders+`)
		 AND status IN ('starting','running','detached')`,
		args...)
	if err != nil {
		return 0, wrapErr(err)
	}
	return res.RowsAffected()
}

func collectSessions(rows *sql.Rows) ([]*SessionMeta, error) {
	var out []*SessionMeta
	for rows.Next() {
		m, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, wrapErr(rows.Err())
}

func scanSession(sc rowScanner) (*SessionMeta, error) {
	var (
		m              SessionMeta
		argsJSON       string
		pid            sql.NullInt64
		exitCode       sql.NullInt64
		cols           int
		rows           int
		createdAt      int64
		startedAt      sql.NullInt64
		endedAt        sql.NullInt64
		lastAttachedAt sql.NullInt64
		createdBy      sql.NullString
		archived       bool
	)
	err := sc.Scan(&m.ID, &m.DeviceID, &m.UserID, &m.Name, &m.Command, &argsJSON,
		&m.Cwd, &m.Status, &pid, &exitCode, &cols, &rows, &createdAt,
		&startedAt, &endedAt, &lastAttachedAt, &createdBy, &archived)
	if err != nil {
		return nil, wrapErr(err)
	}
	m.Args, err = unmarshalArgs(argsJSON)
	if err != nil {
		return nil, err
	}
	if pid.Valid {
		m.PID = int(pid.Int64)
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		m.ExitCode = &v
	}
	// 越界值不该出现，但真出现了也不该 panic —— 截断并交给上层展示。
	m.Cols = clampUint16(cols)
	m.Rows = clampUint16(rows)
	m.CreatedAt = fromMs(createdAt)
	m.StartedAt = msPtr(startedAt)
	m.EndedAt = msPtr(endedAt)
	m.LastAttachedAt = msPtr(lastAttachedAt)
	m.CreatedBy = createdBy.String
	m.Archived = archived
	return &m, nil
}

// marshalArgs 把命令行参数序列化成 JSON。
//
// ★ 必须区分 nil 和空切片：nil → "[]"，[] → "[]"。
// 用 `nil` 会写进 SQL NULL，而列上有 NOT NULL DEFAULT '[]'。
func marshalArgs(args []string) (string, error) {
	if len(args) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("storage: 序列化会话参数失败: %w", err)
	}
	return string(raw), nil
}

func unmarshalArgs(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// 数据损坏不应该让整个列表接口 500 —— 参数是可选信息，
		// 丢掉它比让用户看不到会话列表要好。
		return nil, nil
	}
	return out, nil
}

func clampUint16(v int) uint16 {
	if v <= 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// nullIfZero 把 0 转成 NULL。用于 pid —— 0 不是合法进程号。
func nullIfZero(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// toIntArg 把 *int 转成可绑定参数。nil → NULL。
func toIntArg(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// ---------------------------------------------------------------------------
// audit_logs
// ---------------------------------------------------------------------------

const auditCols = `id, user_id, device_id, session_id, action, result,
	ip, user_agent, meta_json, created_at`

// InsertAudit 写一条审计记录。
//
// ★ 审计失败不应该让业务失败：用户登录成功了，不该因为审计表写不进去
// 就报 500。所以调用方应当 log 错误而不是返回错误 —— 但也不能完全忽略，
// 所以本函数如实返回 error，由调用方决定（通常是 Warn 日志 + 继续）。
func (s *SQLite) InsertAudit(ctx context.Context, a *AuditLog) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs
		 (id, user_id, device_id, session_id, action, result, ip, user_agent, meta_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, nullIfEmpty(a.UserID), nullIfEmpty(a.DeviceID), nullIfEmpty(a.SessionID),
		a.Action, a.Result, nullIfEmpty(a.IP), nullIfEmpty(a.UserAgent),
		nullIfEmpty(a.MetaJSON), toMs(a.CreatedAt),
	)
	return wrapErr(err)
}

// AuditLogsByUser 按时间倒序返回某账号的审计记录。
func (s *SQLite) AuditLogsByUser(ctx context.Context, userID string, limit int) ([]*AuditLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_logs WHERE user_id = ?
		 ORDER BY created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()

	var out []*AuditLog
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, wrapErr(rows.Err())
}

// AuditLogsByUserBefore 是 AuditLogsByUser 的游标版本。
//
// 排序用 id DESC 而不是 created_at DESC：两者都是时间序，但 id 唯一，
// 而游标分页的前提就是「排序键必须唯一」—— 否则同一毫秒的多条记录
// 在翻页边界上会被跳过或重复。UUIDv7 的字典序就是时间序，正好可用。
func (s *SQLite) AuditLogsByUserBefore(ctx context.Context, userID, beforeID string, limit int) ([]*AuditLog, error) {
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT ` + auditCols + ` FROM audit_logs WHERE user_id = ?`
	args := []any{userID}
	if beforeID != "" {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()

	var out []*AuditLog
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, wrapErr(rows.Err())
}

func scanAudit(sc rowScanner) (*AuditLog, error) {
	var (
		a         AuditLog
		userID    sql.NullString
		deviceID  sql.NullString
		sessionID sql.NullString
		ip        sql.NullString
		userAgent sql.NullString
		metaJSON  sql.NullString
		createdAt int64
	)
	err := sc.Scan(&a.ID, &userID, &deviceID, &sessionID, &a.Action, &a.Result,
		&ip, &userAgent, &metaJSON, &createdAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	a.UserID = userID.String
	a.DeviceID = deviceID.String
	a.SessionID = sessionID.String
	a.IP = ip.String
	a.UserAgent = userAgent.String
	a.MetaJSON = metaJSON.String
	a.CreatedAt = fromMs(createdAt)
	return &a, nil
}
