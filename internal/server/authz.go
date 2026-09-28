package server

import (
	"context"
	"errors"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// 本文件是**授权的唯一入口**（§10.5）。
//
// # 纪律
//
// 任何 handler / WS 消息处理都**不允许**自己写 SQL 去查 session/device，
// 必须走这里的函数。这是防 IDOR 最有效的手段 —— 把「查询」和「校验归属」
// 绑在同一个函数里，就不会出现「某处忘了校验」。
//
// # 为什么错误信息要统一
//
// 下面所有「不属于你」的情况都返回与「不存在」**完全相同**的错误码和文案。
// 如果区分开，攻击者就能拿一批 UUID 去探测哪些设备/会话真实存在 ——
// 光是「存在性」本身就已经是信息泄露（能用来画出一家公司有多少台机器）。

// errUnauthenticated 是「没有有效凭证」的统一错误。
func errUnauthenticated() error {
	return protocol.NewError(protocol.CodeUnauthenticated, "未认证")
}

// errNoAccess 是「目标不存在或不属于你」的统一错误。
//
// ★ 用 CodeNotFound（→ HTTP 404）而不是 CodeForbidden（→ 403）。
//
// 403 会说「这东西存在，只是你不能看」，而这句话本身就把存在性泄露了。
// 设备和会话刻意用**同一个错误对象**：调用方不需要、也不应该
// 通过错误内容反推出目标是否存在。
//
// 文案也必须与 writeDeviceError 里那条完全一致 —— 否则响应体长度、
// 内容上的任何差异都是一个可用的预言机。
func errNoAccess() error {
	return protocol.NewError(protocol.CodeNotFound, "目标不存在或无权访问")
}

// authorizeDevice 校验设备属于该用户，返回设备记录。
//
// 这是设备方向所有操作的必经之路。
func (s *Server) authorizeDevice(ctx context.Context, userID, deviceID string) (*storage.Device, error) {
	if userID == "" {
		return nil, errUnauthenticated()
	}
	if deviceID == "" {
		return nil, protocol.NewError(protocol.CodeInvalidPayload, "device_id 为空")
	}

	d, err := s.store.DeviceByID(ctx, deviceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errNoAccess()
		}
		return nil, err
	}

	if d.UserID != userID {
		// 记 Warn：正常的跨账号访问不该发生，出现就说明前端有 bug 或有人在扫 ID。
		s.log.Warn("拒绝跨账号访问设备",
			"user_id", userID, "device_id", deviceID, "owner_id", d.UserID)
		return nil, errNoAccess()
	}
	return d, nil
}

// requireAgent 取某设备**在线**的 Agent 连接，并校验归属。
//
// 返回的错误区分「设备不在线」和「无权访问」是刻意的：
// 能通过 authorizeDevice 的调用者本来就已经有权看到这台设备，
// 告诉他「离线」不会泄露任何新信息，但能让 UI 给出正确的提示。
func (s *Server) requireAgent(ctx context.Context, userID, deviceID string) (*AgentConn, error) {
	if _, err := s.authorizeDevice(ctx, userID, deviceID); err != nil {
		return nil, err
	}

	agent, ok := s.reg.Agent(deviceID)
	if !ok {
		return nil, protocol.NewError(protocol.CodeDeviceOffline, "设备不在线")
	}
	// 双保险：注册表里的连接 owner 也必须匹配。
	// 设备被解绑又绑给别人时，旧连接可能还没被踢掉。
	if agent.UserID != userID {
		s.log.Warn("Agent 连接的 owner 与设备归属不一致",
			"user_id", userID, "device_id", deviceID, "conn_owner", agent.UserID)
		return nil, errNoAccess()
	}
	return agent, nil
}

// authorizeSession 校验会话属于该用户，返回会话元数据。
//
// 归属判定路径：`session.device_id → device.user_id == userID`。
//
// ★ 刻意**不**用 `sessions.user_id`：那是会话创建时的快照。
// 设备被解绑并重新绑定到另一个账号之后，这个快照就过期了 ——
// 用它做授权会留下「前主人仍能操作该设备上的会话」的洞。
//
// 返回的 meta 一定非 nil（实时会话可能还没写回 DB，此时返回一个最小记录）。
func (s *Server) authorizeSession(ctx context.Context, userID, sessionID string) (*storage.SessionMeta, error) {
	if userID == "" {
		return nil, errUnauthenticated()
	}
	if sessionID == "" {
		return nil, protocol.NewError(protocol.CodeInvalidPayload, "session_id 为空")
	}

	// 1. 会话当前挂在哪台设备上。
	//    优先问注册表 —— 它是实时的，且省掉一次 DB 往返。
	deviceID, live := s.reg.SessionOwner(sessionID)

	var meta *storage.SessionMeta
	if !live {
		// 注册表里没有 → 会话不在任何在线 Agent 上。
		// 回退到 DB 的历史记录（用于「查看已结束的会话」这类场景）。
		m, err := s.store.SessionByID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				// 统一成 no_access：不告诉调用者「这个会话压根不存在」
				// 还是「存在但不是你的」。
				return nil, errNoAccess()
			}
			return nil, err
		}
		meta = m
		deviceID = m.DeviceID
	}

	// 2. 那台设备属于这个用户吗。
	if _, err := s.authorizeDevice(ctx, userID, deviceID); err != nil {
		// 把设备层面的错误折叠成会话层面的统一错误，
		// 避免通过错误码差异反推设备是否存在。
		return nil, errNoAccess()
	}

	// 3. 补齐元数据。
	if meta == nil {
		m, err := s.store.SessionByID(ctx, sessionID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		if m != nil {
			meta = m
		} else {
			// 实时会话但 DB 缓存还没写回（session.create 刚发生）。
			// 返回最小记录：调用方需要的主要是 DeviceID 与 ID。
			meta = &storage.SessionMeta{ID: sessionID, DeviceID: deviceID, UserID: userID}
		}
	}
	return meta, nil
}

// authorizeSessionWithDevice 是 authorizeSession 的便利版本，
// 同时返回设备记录（多数 handler 两个都要用）。
func (s *Server) authorizeSessionWithDevice(
	ctx context.Context, userID, sessionID string,
) (*storage.SessionMeta, *storage.Device, error) {
	meta, err := s.authorizeSession(ctx, userID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	d, err := s.store.DeviceByID(ctx, meta.DeviceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, errNoAccess()
		}
		return nil, nil, err
	}
	if d.UserID != userID {
		return nil, nil, errNoAccess()
	}
	return meta, d, nil
}
