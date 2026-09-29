package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/device"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

// deviceDTO 是设备的对外表示。
//
// ★ 显式列出字段，而不是直接序列化 device.View。
//
// View 内嵌了 *storage.Device，直接序列化会把 PublicKey（[]byte → base64）
// 一起吐出去。公钥本身不是秘密，但「把内部模型当 API 契约」是个会持续
// 制造问题的习惯：下次给 Device 加一个内部字段（比如内部备注、
// 风控标记），它会**自动**出现在 API 里，而且没人会注意到。
type deviceDTO struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Platform     string  `json:"platform"`
	Arch         string  `json:"arch"`
	AgentVersion string  `json:"agent_version"`
	Online       bool    `json:"online"`
	Paired       bool    `json:"paired"`
	PairedAt     *string `json:"paired_at,omitempty"`
	LastSeenAt   *string `json:"last_seen_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

func newDeviceDTO(v device.View) deviceDTO {
	return deviceDTO{
		ID:           v.ID,
		Name:         v.Name,
		Platform:     v.Platform,
		Arch:         v.Arch,
		AgentVersion: v.AgentVersion,
		Online:       v.Online,
		Paired:       v.Paired(),
		PairedAt:     rfc3339Ptr(v.PairedAt),
		LastSeenAt:   rfc3339Ptr(v.LastSeenAt),
		CreatedAt:    rfc3339(v.CreatedAt),
	}
}

func newDeviceDTOs(views []device.View) []deviceDTO {
	// 预分配成非 nil：nil 切片会序列化成 `null`，前端得为它单独写一条分支。
	out := make([]deviceDTO, 0, len(views))
	for _, v := range views {
		out = append(out, newDeviceDTO(v))
	}
	return out
}

// rfc3339 把时间转成 RFC3339 字符串（§14.2 的 REST 约定）。
//
// 零值返回空串而不是 "0001-01-01T00:00:00Z"：
// 后者看起来像一个真实时间，前端会把它显示成「公元 1 年」。
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func rfc3339Ptr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// ---------------------------------------------------------------------------
// 设备列表 / 详情 / 改名 / 解绑
// ---------------------------------------------------------------------------

// handleDeviceList 列出当前账号的全部设备。
//
// GET /api/v1/devices   （Bearer）
func (s *Server) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	views, err := s.devices.List(r.Context(), userID)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": newDeviceDTOs(views)})
}

// handleDeviceGet 取设备详情。
//
// GET /api/v1/devices/{id}   （Bearer）
func (s *Server) handleDeviceGet(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	v, err := s.devices.Get(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDeviceDTO(*v))
}

type renameDeviceRequest struct {
	Name string `json:"name"`
}

// handleDeviceRename 改设备显示名。
//
// PATCH /api/v1/devices/{id}   （Bearer）
func (s *Server) handleDeviceRename(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")

	req, ok := decodeJSON[renameDeviceRequest](w, r)
	if !ok {
		return
	}

	v, err := s.devices.Rename(r.Context(), userID, deviceID, req.Name)
	if err != nil {
		writeDeviceError(w, err)
		return
	}

	s.auditRequest(r, auditEntry{
		UserID: userID, DeviceID: deviceID, Action: auditDeviceRename, Result: auditResultOK,
	})
	writeJSON(w, http.StatusOK, newDeviceDTO(*v))
}

// handleDeviceDelete 解绑并删除设备。
//
// DELETE /api/v1/devices/{id}   （Bearer）
//
// 删除后必须**踢掉该设备当前的 Agent 连接**：不踢的话，那台机器上的
// Agent 会继续以为自己已配对，保持一条长连接、继续上报心跳，
// 而服务端已经没有任何记录能授权它 —— 从用户视角看就是
// 「我删了这台设备，但它还在线上」。
func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")

	if err := s.devices.Delete(r.Context(), userID, deviceID); err != nil {
		writeDeviceError(w, err)
		return
	}

	// 4401 而不是 4403：Agent 侧把 4401 判成「不可重连，需重新配对」
	// （internal/agent.IsFatalClose）。用 4403 它只会一直重连，
	// 而每次重连都会被服务端以「设备未注册」拒掉，刷满日志。
	kicked := s.kickAgent(deviceID, CloseUnauthorized, "device_unpaired")

	s.auditRequest(r, auditEntry{
		UserID: userID, DeviceID: deviceID, Action: auditDeviceDelete, Result: auditResultOK,
		Meta: map[string]any{"agent_kicked": kicked},
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleDeviceSessions 返回某设备的会话**缓存视图**（§14.2）。
//
// GET /api/v1/devices/{id}/sessions   （Bearer）
//
// ★ 这是给首屏用的缓存，不是真相源。真相源永远是 Agent 内存里的会话集合；
// 前端拿到这份数据后应当立刻用 WS 的 `session.list` 校正。
// 缓存里的 status 可能落后（Agent 崩了但还没被心跳对账清掉）。
func (s *Server) handleDeviceSessions(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")

	// 走 authorizeDevice 而不是 devices.Get：后者会多做一次在线状态查询，
	// 而这里只需要「这台设备属于我」这一个结论。
	if _, err := s.authorizeDevice(r.Context(), userID, deviceID); err != nil {
		writeProtoError(w, err)
		return
	}

	metas, err := s.store.SessionsByDevice(r.Context(), deviceID)
	if err != nil {
		writeProtoError(w, err)
		return
	}

	out := make([]protocol.SessionSummary, 0, len(metas))
	for _, m := range metas {
		out = append(out, sessionSummary(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// sessionSummary 把存储层的会话元数据转成协议层的对外快照。
//
// 复用 protocol.SessionSummary 而不是另造一个 DTO：这个结构同时出现在
// Agent 上报、Server 转发、REST 响应三处，三处用同一个类型能保证
// 前端只写一套解析逻辑。
func sessionSummary(m *storage.SessionMeta) protocol.SessionSummary {
	return protocol.SessionSummary{
		SessionID:      m.ID,
		DeviceID:       m.DeviceID,
		Name:           m.Name,
		Command:        m.Command,
		Args:           m.Args,
		Cwd:            m.Cwd,
		Status:         m.Status,
		PID:            m.PID,
		ExitCode:       m.ExitCode,
		Cols:           m.Cols,
		Rows:           m.Rows,
		CreatedAt:      m.CreatedAt.UnixMilli(),
		StartedAt:      msPtrOf(m.StartedAt),
		EndedAt:        msPtrOf(m.EndedAt),
		LastAttachedAt: msPtrOf(m.LastAttachedAt),
		// BufferSeqFrom/To 留 0：ring buffer 在 Agent 内存里，
		// Server 不知道它的边界。前端 attach 时会从 session.attached
		// 里拿到真实区间。
	}
}

func msPtrOf(t *time.Time) *int64 {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

// ---------------------------------------------------------------------------
// 配对（§10.3）
// ---------------------------------------------------------------------------

type pairRequest struct {
	Code string `json:"code"`
}

type pairConfirmRequest struct {
	Code string `json:"code"`
	// Name 允许用户在确认页顺手改个名字。
	// 为空表示沿用 Agent 自报的名字。
	Name string `json:"name,omitempty"`
}

// pairPreviewResponse 是「输入配对码之后、点确认之前」给用户看的信息。
//
// ★ 这个响应是两阶段确认的核心（§10.4）。
//
// 猜中一个配对码不等于能绑定成功 —— 用户会先看到这台机器的名字、
// 平台和它连接的 IP。不是自己的机器就点取消。没有这一步的话，
// 猜中码 = 直接把别人的电脑绑到自己账号上。
type pairPreviewResponse struct {
	DeviceID      string `json:"device_id"`
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	AgentVersion  string `json:"agent_version"`
	AgentIP       string `json:"agent_ip"`
	AlreadyPaired bool   `json:"already_paired"`
	ExpiresAt     string `json:"expires_at"`
	// AgentOnline 表示 Agent 此刻是否还挂着等确认。
	// 为 false 时用户应当知道「确认后 Agent 那边可能不会立刻收到通知」。
	AgentOnline bool `json:"agent_online"`
}

// handleDevicePair 第一步：提交配对码，拿到待确认的设备信息。
//
// POST /api/v1/devices/pair   （Bearer）
func (s *Server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	req, ok := decodeJSON[pairRequest](w, r)
	if !ok {
		return
	}

	pc, ok := s.checkPairingCode(w, r, userID, req.Code)
	if !ok {
		return
	}

	d, err := s.store.DeviceByID(r.Context(), pc.DeviceID)
	already := false
	switch {
	case err == nil:
		// 已绑定到**别人**：直接拒绝。
		//
		// 这一步必须在预览阶段就挡住，不能等到 confirm ——
		// 否则用户会看到一个「确认绑定」按钮，点了才发现不行。
		if d.UserID != "" && d.UserID != userID {
			s.pairFailure(r, userID, "device_owned_by_other")
			s.auditRequest(r, auditEntry{
				UserID: userID, DeviceID: pc.DeviceID,
				Action: auditDevicePairFail, Result: auditResultDenied,
				Meta: map[string]any{"reason": "owned_by_other"},
			})
			writeError(w, http.StatusConflict, string(protocol.CodeForbidden), "该设备已绑定到其他账号")
			return
		}
		already = d.UserID == userID
	case errors.Is(err, storage.ErrNotFound):
		// 还没建记录 —— 正常路径（配对成功后才会写 devices 表）。
	default:
		writeProtoError(w, err)
		return
	}

	s.auditRequest(r, auditEntry{
		UserID: userID, DeviceID: pc.DeviceID,
		Action: auditDevicePairBegin, Result: auditResultOK,
		Meta: map[string]any{"already_paired": already},
	})

	writeJSON(w, http.StatusOK, pairPreviewResponse{
		DeviceID:      pc.DeviceID,
		Name:          pc.Name,
		Platform:      pc.Platform,
		Arch:          pc.Arch,
		AgentVersion:  pc.AgentVersion,
		AgentIP:       pc.AgentIP,
		AlreadyPaired: already,
		ExpiresAt:     rfc3339(pc.ExpiresAt),
		AgentOnline:   s.pairs.Waiting(pc.DeviceID),
	})
}

// handleDevicePairConfirm 第二步：确认绑定。
//
// POST /api/v1/devices/pair/confirm   （Bearer）
func (s *Server) handleDevicePairConfirm(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	req, ok := decodeJSON[pairConfirmRequest](w, r)
	if !ok {
		return
	}

	pc, ok := s.checkPairingCode(w, r, userID, req.Code)
	if !ok {
		return
	}

	ctx := r.Context()
	now := s.now()

	// ---- 1. 确保 devices 表里有这条记录 ----
	d, err := s.store.DeviceByID(ctx, pc.DeviceID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		// 首次配对：设备记录在此刻才诞生。
		//
		// 名字优先用用户在确认页填的；没填就用 Agent 自报的。
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = pc.Name
		}
		d = &storage.Device{
			ID:           pc.DeviceID,
			Name:         name,
			Platform:     pc.Platform,
			Arch:         pc.Arch,
			AgentVersion: pc.AgentVersion,
			PublicKey:    pc.PublicKey,
			CreatedAt:    now,
		}
		if err := s.store.CreateDevice(ctx, d); err != nil {
			writeProtoError(w, err)
			return
		}
	case err != nil:
		writeProtoError(w, err)
		return
	default:
		// 记录已存在。
		if d.UserID != "" && d.UserID != userID {
			s.pairFailure(r, userID, "device_owned_by_other")
			writeError(w, http.StatusConflict, string(protocol.CodeForbidden), "该设备已绑定到其他账号")
			return
		}
		// 未绑定的旧记录：把 Agent 这次自报的信息覆盖上去。
		//
		// 覆盖公钥是必须的 —— Agent 重装会重新生成密钥对，
		// 而配对码里带的就是**这次**的公钥。不覆盖的话，
		// 配对成功但之后 Ed25519 认证会失败（服务端还拿着旧公钥）。
		if req.Name != "" {
			if err := s.store.RenameDevice(ctx, pc.DeviceID, strings.TrimSpace(req.Name)); err != nil {
				writeProtoError(w, err)
				return
			}
		}
	}

	// ---- 2. 绑定 ----
	// 已经在自己名下就不重复绑（幂等）：用户可能刷新页面又点了一次确认。
	if d.UserID != userID {
		if err := s.devices.Bind(ctx, userID, pc.DeviceID); err != nil {
			if errors.Is(err, device.ErrAlreadyPaired) {
				// 并发确认：另一个请求刚刚绑给了别人。
				s.pairFailure(r, userID, "already_paired")
				writeError(w, http.StatusConflict, string(protocol.CodeForbidden), "该设备已绑定到其他账号")
				return
			}
			writeProtoError(w, err)
			return
		}
	}

	// ---- 3. 作废配对码 ----
	//
	// ★ 放在绑定**之后**：反过来的话，标记成功但绑定失败（比如并发冲突）
	// 会让配对码白白消耗掉，用户必须回 Agent 重新生成一个。
	//
	// ErrConflict 说明另一个请求已经用掉了这个码 —— 那正是单次使用的语义，
	// 但此时设备可能已经被绑给别人，所以要如实报错而不是假装成功。
	if err := s.store.MarkPairingCodeUsed(ctx, pc.ID, userID, now); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			s.pairFailure(r, userID, "code_already_used")
			writeError(w, http.StatusConflict, string(protocol.CodeInvalidPayload), "配对码已被使用")
			return
		}
		writeProtoError(w, err)
		return
	}

	// 成功后清掉失败计数：同一用户偶发输错几次不该累积到锁定。
	s.pairLimit.Reset("u:" + userID)
	s.pairLimit.Reset("ip:" + s.clientIP(r))

	s.auditRequest(r, auditEntry{
		UserID: userID, DeviceID: pc.DeviceID,
		Action: auditDevicePairConfirm, Result: auditResultOK,
		Meta: map[string]any{"name": d.Name, "platform": pc.Platform},
	})

	// ---- 4. 通知 Agent ----
	//
	// 通知失败**不算失败**：Agent 可能已经等超时断开了（配对码在浏览器
	// 侧仍然有效），此时绑定照样成立。Agent 下次跑 `run` 时会发现
	// 自己已被绑定，直接进入正常连接状态机。
	notified := s.notifyPairCompleted(ctx, pc.DeviceID, userID)

	v, err := s.devices.Get(ctx, userID, pc.DeviceID)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":         newDeviceDTO(*v),
		"agent_notified": notified,
	})
}

// checkPairingCode 是 pair / pair-confirm 共用的前置校验。
//
// 返回 ok=false 表示已经写过响应了。
//
// # 校验顺序刻意固定
//
// 格式 → 锁定 → 查库 → 可用性。从廉价到昂贵，而且**任何一次失败都
// 记一次失败计数** —— 包括格式错的那些。理由：攻击者猜码时大概率
// 会试到不存在的组合，如果只对「查到了但过期」计数，
// 连续失败锁定就形同虚设。
func (s *Server) checkPairingCode(
	w http.ResponseWriter, r *http.Request, userID, raw string,
) (*storage.PairingCode, bool) {
	ip := s.clientIP(r)

	// ★ 锁定检查用两个维度：按用户、按 IP。
	//
	// 只按用户：攻击者可以注册一堆账号轮流试（allow_signup 打开时）。
	// 只按 IP：攻击者换 IP 就绕过，而受害者自己被锁在同一出口 IP 上。
	// 两者叠加才是「锁得住攻击者、不误伤邻居」。
	if s.pairLimit.Locked("u:"+userID) || s.pairLimit.Locked("ip:"+ip) {
		writeError(w, http.StatusTooManyRequests, string(protocol.CodeRateLimited),
			"配对尝试次数过多，请稍后再试")
		return nil, false
	}

	// 格式不对直接拒，不查库 —— 省一次 DB 往返，也避免无意义的 code_hash 查询。
	if !auth.ValidPairingCodeFormat(raw) {
		s.pairFailure(r, userID, "bad_format")
		s.auditRequest(r, auditEntry{
			UserID: userID, Action: auditDevicePairFail, Result: auditResultDenied,
			Meta: map[string]any{"reason": "bad_format"},
		})
		// ★ 与「码不存在」返回**完全相同**的错误。
		// 分开的话，攻击者可以先用格式校验把搜索空间砍掉一大块。
		writeError(w, http.StatusNotFound, string(protocol.CodeInvalidPayload), "配对码无效或已过期")
		return nil, false
	}

	pc, err := s.store.PairingCodeByHash(r.Context(), auth.HashPairingCode(raw))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.pairFailure(r, userID, "not_found")
			s.auditRequest(r, auditEntry{
				UserID: userID, Action: auditDevicePairFail, Result: auditResultDenied,
				Meta: map[string]any{"reason": "not_found"},
			})
			writeError(w, http.StatusNotFound, string(protocol.CodeInvalidPayload), "配对码无效或已过期")
			return nil, false
		}
		writeProtoError(w, err)
		return nil, false
	}

	if !pc.Usable(s.now()) {
		s.pairFailure(r, userID, "expired_or_used")
		s.auditRequest(r, auditEntry{
			UserID: userID, DeviceID: pc.DeviceID,
			Action: auditDevicePairFail, Result: auditResultDenied,
			Meta: map[string]any{"reason": "expired_or_used"},
		})
		// 已用/过期同样折叠成「无效」。
		//
		// ★ 特别不能区分「已使用」和「不存在」：那会让攻击者知道
		// 自己猜中了一个**曾经有效**的码 —— 这本身就是巨大的信息泄露
		// （说明他的搜索方向对了）。
		writeError(w, http.StatusNotFound, string(protocol.CodeInvalidPayload), "配对码无效或已过期")
		return nil, false
	}

	return pc, true
}

// pairFailure 记一次配对失败（两个维度都记）。
func (s *Server) pairFailure(r *http.Request, userID, reason string) {
	s.pairLimit.RecordFailure("u:" + userID)
	s.pairLimit.RecordFailure("ip:" + s.clientIP(r))
	s.log.Warn("配对失败", "user_id", userID, "reason", reason, "ip", s.clientIP(r))
}

// notifyPairCompleted 给等待中的 Agent 发 `agent.pair.completed`。
func (s *Server) notifyPairCompleted(ctx context.Context, deviceID, userID string) bool {
	// 带 email 是为了让 Agent 终端能打印「已绑定到 jojo@example.com」——
	// 用户在多账号环境下需要确认绑对了哪个账号。
	//
	// 查不到用户不阻断配对：绑定已经落库了，只是少打印一行提示。
	var email string
	if u, err := s.store.UserByID(ctx, userID); err == nil {
		email = u.Email
	}

	env, err := protocol.NewEnvelope(protocol.TypeAgentPairCompleted, protocol.AgentPairCompletedPayload{
		DeviceID:  deviceID,
		UserEmail: email,
	})
	if err != nil {
		s.log.Error("构造 pair.completed 失败", "device_id", deviceID, "err", err)
		return false
	}
	return s.pairs.Notify(deviceID, env)
}

// ---------------------------------------------------------------------------
// 审计日志
// ---------------------------------------------------------------------------

// auditDTO 是审计记录的对外表示。
type auditDTO struct {
	ID        string          `json:"id"`
	Action    string          `json:"action"`
	Result    string          `json:"result"`
	DeviceID  string          `json:"device_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	IP        string          `json:"ip,omitempty"`
	UserAgent string          `json:"user_agent,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// defaultAuditLimit / maxAuditLimit 是分页的上下界。
//
// 上限存在的意义：`?limit=1000000` 会让服务端去拼一个巨大的响应，
// 而这是个纯读取的 DoS 面。有上限之后，想翻完只能靠游标，
// 而游标翻页的成本是线性的、可控的。
const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

// handleAuditLogs 分页返回当前账号的审计日志（§14.2）。
//
// GET /api/v1/audit?limit=&cursor=   （Bearer）
func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	limit := parseLimit(r.URL.Query().Get("limit"), defaultAuditLimit, maxAuditLimit)

	cursor, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), "cursor 不合法")
		return
	}

	logs, err := s.store.AuditLogsByUserBefore(r.Context(), userID, cursor, limit)
	if err != nil {
		writeProtoError(w, err)
		return
	}

	items := make([]auditDTO, 0, len(logs))
	for _, a := range logs {
		items = append(items, auditDTO{
			ID:        a.ID,
			Action:    a.Action,
			Result:    a.Result,
			DeviceID:  a.DeviceID,
			SessionID: a.SessionID,
			IP:        a.IP,
			UserAgent: a.UserAgent,
			Meta:      rawIfValid(a.MetaJSON),
			CreatedAt: rfc3339(a.CreatedAt),
		})
	}

	resp := map[string]any{"items": items}
	// 只有「拿满了」才给游标。返回不满一页说明已经到底了，
	// 此时再给一个游标只会让前端多打一次空请求。
	if len(logs) == limit && len(logs) > 0 {
		resp["next_cursor"] = encodeCursor(logs[len(logs)-1].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// rawIfValid 只在字符串确实是合法 JSON 时才把它当 JSON 嵌入。
//
// 直接塞非法 JSON 会让**整个响应**序列化失败 —— 一条损坏的审计记录
// 就能让审计列表接口整体 500。丢掉那一条的 meta 显然好得多。
func rawIfValid(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// encodeCursor / decodeCursor 把不透明游标包一层 base64。
//
// 包这一层不是为了保密（ID 本来就会返回给前端），而是为了**保留
// 改实现的自由**：将来游标换成 (created_at, id) 复合键时，
// 前端不用改任何代码。
func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// parseLimit 解析 limit 参数，越界一律夹到合法范围。
//
// ★ 不报错而是夹紧：`?limit=0` 或 `?limit=abc` 更可能是前端拼错了，
// 而不是恶意 —— 为此返回 400 只会让页面白屏。上限必须夹（DoS 面），
// 下限没必要。
func parseLimit(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// writeDeviceError 把 device 包的领域错误映射成 HTTP 响应。
//
// 集中在一个函数里，是为了避免「同一个错误在 A 端点返回 404、
// 在 B 端点返回 403」这类不一致 —— 那会让前端不得不为每个端点
// 写一套分支，而这些分支迟早会漏掉一个。
func writeDeviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, device.ErrNotFound):
		// ★ 与 authz.errNoAccess 保持**逐字节一致**。
		//
		// 这两条路径覆盖了同一件事：devices.Get 走这里，
		// devices/{id}/sessions 走 authorizeDevice → errNoAccess。
		// 任何差异（状态码、错误码、文案）都会让调用者能区分
		// 「设备不存在」和「设备是别人的」—— 那就是一个存在性预言机。
		writeError(w, http.StatusNotFound, string(protocol.CodeNotFound), "目标不存在或无权访问")
	case errors.Is(err, device.ErrEmptyName), errors.Is(err, device.ErrNameTooLong):
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), err.Error())
	case errors.Is(err, device.ErrAlreadyPaired):
		writeError(w, http.StatusConflict, string(protocol.CodeForbidden), "该设备已绑定到其他账号")
	default:
		writeProtoError(w, err)
	}
}

// handleDeviceUpdateStatus reports the last status from the currently connected
// Agent. A missing status means no heartbeat has arrived yet, not "up to date".
func (s *Server) handleDeviceUpdateStatus(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), userID, deviceID); err != nil {
		writeProtoError(w, err)
		return
	}
	if agent, ok := s.reg.Agent(deviceID); ok {
		writeJSON(w, http.StatusOK, map[string]any{"online": true, "agent_version": agent.AgentVersion(), "update": agent.UpdateStatus(), "commands": agent.Commands()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": false, "update": nil, "commands": []protocol.CommandAvailability{}})
}
