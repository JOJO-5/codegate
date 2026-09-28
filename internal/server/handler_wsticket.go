package server

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// wsTicketTTL 是一次性票据的有效期。
//
// 30 秒刻意很短：票据的唯一用途是「把 Authorization 头换成 WS 能用的凭证」
// （浏览器的 WebSocket API 不支持自定义头）。它的生命周期只需要覆盖
// 「拿到票据 → 发起连接」这一瞬间。
const wsTicketTTL = 30 * time.Second

// ticketStore 保存一次性 WS 票据。
//
// 存在内存里而不是数据库：票据活 30 秒且用完即焚，
// 落库要付一次写 + 一次读 + 一次删，还要处理多实例共享 ——
// 完全不值得。多实例部署时用 sticky session 或把票据换成 JWT 即可。
type ticketStore struct {
	mu   sync.Mutex
	data map[string]*ticket
}

type ticket struct {
	userID    string
	expiresAt time.Time
}

func newTicketStore() *ticketStore {
	return &ticketStore{data: make(map[string]*ticket)}
}

// Issue 签发一张票据。
func (t *ticketStore) Issue(userID string, now time.Time) (string, time.Duration, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, fmt.Errorf("server: 生成 WS 票据失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	t.mu.Lock()
	t.data[token] = &ticket{userID: userID, expiresAt: now.Add(wsTicketTTL)}
	t.mu.Unlock()

	return token, wsTicketTTL, nil
}

// Consume 校验并**立即作废**票据。
//
// ★ 无论票据是否已过期，只要存在就先删掉再判断。
//
// 反过来写（先判断有效性，无效就不删）会让一张过期票据留在 map 里，
// 虽然它已经不可用，但更重要的是：**「存在即删除」让这个操作天然幂等**，
// 两个并发请求里必然只有一个能拿到 userID。
//
// 真正的重放防护来自这里 —— 票据只在内存里、用完即删，
// 攻击者即使从日志里读到票据也已经无效。
func (t *ticketStore) Consume(token string, now time.Time) (string, bool) {
	if token == "" {
		return "", false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	tk, ok := t.data[token]
	if !ok {
		return "", false
	}
	delete(t.data, token)

	if now.After(tk.expiresAt) {
		return "", false
	}
	return tk.userID, true
}

// sweep 清理过期票据。由后台清理循环调用。
func (t *ticketStore) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, tk := range t.data {
		if now.After(tk.expiresAt) {
			delete(t.data, k)
		}
	}
}

// Len 返回当前票据数量（诊断用）。
func (t *ticketStore) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.data)
}

// ---------------------------------------------------------------------------

// handleWSTicket 签发一次性 WebSocket 票据（§8.2）。
//
// POST /api/v1/ws-ticket
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "未认证")
		return
	}

	token, ttl, err := s.tickets.Issue(userID, s.now())
	if err != nil {
		writeProtoError(w, err)
		return
	}

	// 顺带把协议版本告诉前端，让它在建连之前就能拒绝不兼容的页面（§9.4）。
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     token,
		"expires_in": int(ttl.Seconds()),
		"protocol":   protocol.Current(),
	})
}
