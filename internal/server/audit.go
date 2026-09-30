package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/storage"
)

// 审计动作名。
//
// 用常量而不是散落的字符串字面量：否则查询审计日志时会发现
// 同一个动作有 `login.ok` / `login_success` / `LoginOK` 三种写法，
// 而审计日志的价值恰恰在于「能按动作聚合」。
const (
	auditLoginOK            = "login.ok"
	auditLoginFail          = "login.fail"
	auditRegister           = "user.register"
	auditLogout             = "logout"
	auditPasswordChange     = "password.change"
	auditTokenRefresh       = "token.refresh"
	auditTokenReuse         = "token.reuse_detected"
	auditDevicePairBegin    = "device.pair.begin"
	auditDevicePairConfirm  = "device.pair.confirm"
	auditDevicePairFail     = "device.pair.fail"
	auditDeviceRename       = "device.rename"
	auditDeviceDelete       = "device.delete"
	auditDeviceToolSet      = "device.tool.set"
	auditAgentConnect       = "agent.connect"
	auditAgentAuthFail      = "agent.auth.fail"
	auditAgentDuplicateConn = "agent.duplicate_connection"
	auditSessionCreate      = "session.create"
	auditSessionClose       = "session.close"
)

const (
	auditResultOK     = "ok"
	auditResultDenied = "denied"
	auditResultError  = "error"
)

// maxUserAgentLen 是 User-Agent 入库前的截断长度。
//
// 不截断的话，一个 1 MB 的 User-Agent 头就能往审计表里塞一条 1 MB 的记录 ——
// 而且攻击者可以反复发，把表撑爆。
const maxUserAgentLen = 256

// auditEntry 是一条待写入的审计记录。
//
// ★ Meta 里**绝不允许**出现 token / 密码 / 终端内容（§13.1）。
// 这不是靠自觉：日志脱敏（internal/logging）覆盖的是日志，
// 而审计是直接落库的，所以这里的纪律必须由写代码的人守住。
type auditEntry struct {
	UserID    string
	DeviceID  string
	SessionID string
	Action    string
	Result    string
	IP        string
	UserAgent string
	Meta      map[string]any
}

// audit 写一条审计记录。
//
// ★ 审计失败**不**让业务失败。
//
// 用户登录成功了，不该因为审计表写不进去（磁盘满、锁冲突）就报 500 ——
// 那会把一个可观测性问题放大成可用性事故。所以这里只记 Warn。
// 代价是「审计可能丢」，这个取舍在 MVP 阶段是可接受的。
func (s *Server) audit(ctx context.Context, e auditEntry) {
	meta := ""
	if len(e.Meta) > 0 {
		if raw, err := json.Marshal(e.Meta); err == nil {
			meta = string(raw)
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		// 生成不了 UUID 就没法写审计。极罕见，记一句算了。
		s.log.Warn("生成审计记录 ID 失败", "action", e.Action, "err", err)
		return
	}

	err = s.store.InsertAudit(ctx, &storage.AuditLog{
		ID:        id.String(),
		UserID:    e.UserID,
		DeviceID:  e.DeviceID,
		SessionID: e.SessionID,
		Action:    e.Action,
		Result:    e.Result,
		IP:        e.IP,
		UserAgent: e.UserAgent,
		MetaJSON:  meta,
		CreatedAt: s.now(),
	})
	if err != nil {
		s.log.Warn("写审计失败", "action", e.Action, "result", e.Result, "err", err)
	}
}

// auditRequest 从 HTTP 请求补齐 IP / User-Agent 后写审计。
func (s *Server) auditRequest(r *http.Request, e auditEntry) {
	e.IP = s.clientIP(r)
	e.UserAgent = truncate(e.UserAgent, maxUserAgentLen)
	if e.UserAgent == "" {
		e.UserAgent = truncate(r.UserAgent(), maxUserAgentLen)
	}
	s.audit(r.Context(), e)
}

// truncate 按**字节**截断字符串。
//
// 用字节而不是 rune：目的是限制存储体积，而存储是按字节算的。
// 截断可能切碎一个多字节字符 —— 对 User-Agent 这种 ASCII 为主的
// 场景可以接受，而且后续用 ReplaceAll 清掉孤立的无效字节没必要。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
