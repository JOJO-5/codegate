package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

const (
	// refreshCookieName 是刷新令牌的 Cookie 名。
	//
	// 加 `cg_` 前缀：同一台机器上可能同时跑着别的服务，
	// 一个通用的 `refresh_token` 名字迟早会撞。
	refreshCookieName = "cg_refresh"

	// refreshCookiePath 限定 Cookie 只发给认证端点（§13.3）。
	//
	// ★ 不设成 `/`。Path=/ 意味着浏览器会把刷新令牌附在**每一个**请求上，
	// 包括那些打日志的、被 CDN 缓存的、将来可能被塞进第三方组件的路径。
	// 令牌只在 /api/v1/auth/* 需要，就只发给那里。
	refreshCookiePath = "/api/v1/auth"

	// maxEmailLen 是邮箱长度上限（RFC 5321 的 path 上限是 254）。
	maxEmailLen = 254
)

// ---------------------------------------------------------------------------
// 请求 / 响应 DTO
// ---------------------------------------------------------------------------

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// tokenResponse 是登录/注册/刷新的统一响应。
//
// ★ 刻意**不含 refresh_token 字段**：刷新令牌只走 HttpOnly Cookie，
// 一旦出现在 JSON 里，前端就一定会有人把它存进 localStorage ——
// 那等于把「Cookie 防 XSS」这条设计整个作废。
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// meResponse 是当前用户视图。**不含 password_hash** —— 那是显然的，
// 但更重要的是别把 storage.User 直接序列化出去：将来给 User 加字段时
// 一个 `json:"-"` 忘了加就是一次泄露。显式构造 DTO 是对这个风险的免疫。
type meResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

// handleRegister 注册账号。
//
// POST /api/v1/auth/register
//
// 开放条件（§14.2）：`allow_signup: true`，**或者系统里一个用户都没有**。
// 后一条是引导逻辑：否则全新部署的实例永远无法创建第一个账号，
// 只能靠改环境变量重启 —— 而那个变量往往在容器里，改起来比登天还难。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[registerRequest](w, r)
	if !ok {
		return
	}

	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), "邮箱格式不正确")
		return
	}

	// 是否允许注册。
	//
	// ★ 先查 CountUsers 再决定，而不是把两个条件写进一个 if：
	// 「已有用户 + 不允许注册」与「已有用户 + 允许注册」要给出不同的响应
	// （前者 403 且**不消耗**任何限流额度）。
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if n > 0 && !s.cfg.AllowSignup {
		writeError(w, http.StatusForbidden, string(protocol.CodeForbidden), "本实例未开放注册")
		return
	}

	// 注册也要限流：否则开放注册的实例可以被脚本刷出几十万个账号，
	// 把 users 表撑大、把 bcrypt/argon2 的 CPU 吃满。
	if !s.registerAllowed(r) {
		writeError(w, http.StatusTooManyRequests, string(protocol.CodeRateLimited), "请求过于频繁，请稍后再试")
		return
	}

	if err := auth.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), err.Error())
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeProtoError(w, err)
		return
	}

	now := s.now()
	user := &storage.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: hash,
		Role:         "user",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			// ★ 这里必须回 409 而不是「已存在该邮箱」的 200/400 混淆。
			// 注册接口天然能枚举邮箱，这是无法避免的取舍；
			// 我们选择明确告诉调用者，因为「注册失败但不知道为什么」
			// 会让正常用户反复重试。
			writeError(w, http.StatusConflict, string(protocol.CodeInvalidPayload), "该邮箱已被注册")
			return
		}
		writeProtoError(w, err)
		return
	}

	s.auditRequest(r, auditEntry{
		UserID: user.ID, Action: auditRegister, Result: auditResultOK,
		Meta: map[string]any{"first_user": n == 0},
	})

	// 注册成功直接发凭证：多一次「注册完再登录」的往返没有意义，
	// 而密码刚刚由用户输入，不存在「拿不到密码」的问题。
	if err := s.issueSession(w, r, user); err != nil {
		writeProtoError(w, err)
		return
	}
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

// handleLogin 登录。
//
// POST /api/v1/auth/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[loginRequest](w, r)
	if !ok {
		return
	}

	email := normalizeEmail(req.Email)
	ip := s.clientIP(r)

	// ★ 两个维度的限流都要过。
	//
	// 只按 email：攻击者用一个固定 email 打过来会锁死这个账号（对受害者是 DoS），
	//            但换 email 就能无限试，爆破别人的账号照样成立。
	// 只按 IP：   NAT 后面的一整个公司共用一个出口 IP，一个人手抖就全员登录失败；
	//             而攻击者用肉鸡池换 IP 就绕过了。
	// 两者叠加才是「既锁得住攻击者、又不误伤邻居」。
	if !s.loginLimit.Allow("email:"+email) || !s.loginIPLimit.Allow("ip:"+ip) {
		s.auditRequest(r, auditEntry{
			Action: auditLoginFail, Result: auditResultDenied,
			Meta: map[string]any{"email": email, "reason": "rate_limited"},
		})
		writeError(w, http.StatusTooManyRequests, string(protocol.CodeRateLimited), "登录尝试过于频繁，请稍后再试")
		return
	}

	u, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// ★ 抹平耗时差异。
			//
			// 用户不存在时我们不做 argon2 运算就返回，比「用户存在但密码错」
			// 快两个数量级。攻击者用响应时间就能枚举出哪些邮箱注册过 ——
			// 而我们刚刚才决定「不告诉调用者邮箱是否存在」，
			// 一个 100ms 的时间差会把那个决定完全作废。
			s.burnPasswordTime(req.Password)
			s.loginFailed(w, r, email, "user_not_found")
			return
		}
		writeProtoError(w, err)
		return
	}

	match, err := auth.VerifyPassword(u.PasswordHash, req.Password)
	if err != nil {
		// 哈希串损坏：这是**服务端**的数据问题，不是登录失败。
		// 记 error 级审计，回 500 —— 绝不能伪装成 401，
		// 否则这个账号会永远登不上，而日志里只有一堆 login.fail。
		s.auditRequest(r, auditEntry{
			UserID: u.ID, Action: auditLoginFail, Result: auditResultError,
			Meta: map[string]any{"reason": "hash_corrupt"},
		})
		writeError(w, http.StatusInternalServerError, string(protocol.CodeInternal), "账号数据异常，请联系管理员")
		return
	}
	if !match {
		s.loginFailed(w, r, email, "bad_password")
		return
	}

	// ★ 禁用检查放在密码验证**之后**。
	//
	// 放在前面的话，「这个账号被禁用了」就成了一个不需要密码的查询接口：
	// 攻击者能据此知道哪些账号存在且被封。密码对了他才知道自己本来就有权限，
	// 此时告诉他「已被禁用」不泄露任何他无权知道的信息。
	if u.Disabled {
		s.auditRequest(r, auditEntry{
			UserID: u.ID, Action: auditLoginFail, Result: auditResultDenied,
			Meta: map[string]any{"reason": "disabled"},
		})
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "账号已被禁用")
		return
	}

	// 密码对了，顺手把过时的哈希参数升级掉（§10.1）。
	// 失败不影响登录 —— 这只是一次优化，不该让用户登不进来。
	if auth.NeedsRehash(u.PasswordHash) {
		if h, err := auth.HashPassword(req.Password); err == nil {
			if err := s.store.UpdateUserPassword(r.Context(), u.ID, h, s.now()); err != nil {
				s.log.Warn("升级密码哈希参数失败", "user_id", u.ID, "err", err)
			}
		}
	}

	// 登录成功：清掉这个 email 的失败计数，让「偶发输错几次」不累积。
	s.loginLimit.Reset("email:" + email)

	s.auditRequest(r, auditEntry{UserID: u.ID, Action: auditLoginOK, Result: auditResultOK})

	if err := s.issueSession(w, r, u); err != nil {
		writeProtoError(w, err)
		return
	}
}

// loginFailed 是「凭据不对」的统一出口。
//
// ★ 所有失败原因对外**同一句话、同一个状态码**。
// 分开写的话，攻击者可以靠错误文案区分「邮箱不存在」和「密码错误」，
// 从而先枚举出有效邮箱、再集中爆破。
func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, email, reason string) {
	s.auditRequest(r, auditEntry{
		Action: auditLoginFail, Result: auditResultDenied,
		Meta: map[string]any{"email": email, "reason": reason},
	})
	writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "邮箱或密码不正确")
}

// burnPasswordTime 消耗一次 argon2 运算的时间，用于抹平时间侧信道。
//
// 哈希串在首次调用时生成一次（约 100ms），之后复用。
// 用 sync.Once 而不是包初始化：包初始化会拖慢每一次进程启动，
// 包括那些根本不跑 HTTP 的场景（CLI 的 config/status 子命令）。
var (
	dummyHashOnce sync.Once
	dummyHash     string
)

func (s *Server) burnPasswordTime(password string) {
	dummyHashOnce.Do(func() {
		if h, err := auth.HashPassword("codegate-timing-equalizer"); err == nil {
			dummyHash = h
		}
	})
	if dummyHash == "" {
		return
	}
	_, _ = auth.VerifyPassword(dummyHash, password)
}

// ---------------------------------------------------------------------------
// 刷新
// ---------------------------------------------------------------------------

// handleRefresh 用 Cookie 里的刷新令牌换一对新凭证。
//
// POST /api/v1/auth/refresh
//
// # 轮换 + 重用检测（§10.1）
//
// 每次刷新都换发一个新刷新令牌，旧的同时作废。于是「同一个令牌被用两次」
// 只有一个可能：有人复制了它。此时**整族吊销** —— 合法用户和攻击者
// 都会被踢下线，这是唯一安全的响应：我们分不清谁是攻击者，
// 而放过一个可能的窃贼比让用户重新登录一次糟糕得多。
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	plain, err := readRefreshCookie(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "缺少刷新令牌")
		return
	}

	ctx := r.Context()
	now := s.now()

	rec, err := s.store.RefreshTokenByHash(ctx, auth.HashRefreshToken(plain))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// 查不到：可能是伪造的，也可能是过期记录已被后台清理。
			// 两种情况对外一样，顺手清掉浏览器上那个没用的 Cookie。
			s.clearRefreshCookie(w)
			writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "刷新令牌无效")
			return
		}
		writeProtoError(w, err)
		return
	}

	// 已用过 = 重放信号。
	if rec.UsedAt != nil {
		s.revokeFamilyAndAudit(ctx, r, rec, "reused")
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "刷新令牌已失效，请重新登录")
		return
	}

	if !rec.Valid(now) {
		// 已吊销或已过期。**不是**重用，所以不吊销整族 ——
		// 把正常过期也当成攻击会让用户每次登录一个月后都被强制重登。
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "刷新令牌已过期")
		return
	}

	// ★ 原子地标记已用。
	//
	// 上面的 Valid 检查到这里的 UPDATE 之间有窗口：两个并发请求
	// 可能都读到 UsedAt == nil。MarkRefreshTokenUsed 的 WHERE 带
	// `used_at IS NULL`，把这一步变成比较并交换 —— 只有一个能成功。
	// 失败的说明另一个请求刚刚轮换过，这正是重用场景，按攻击处理。
	if err := s.store.MarkRefreshTokenUsed(ctx, rec.ID, now); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			s.revokeFamilyAndAudit(ctx, r, rec, "concurrent_reuse")
			s.clearRefreshCookie(w)
			writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "刷新令牌已失效，请重新登录")
			return
		}
		writeProtoError(w, err)
		return
	}

	u, err := s.store.UserByID(ctx, rec.UserID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.clearRefreshCookie(w)
			writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "账号不存在")
			return
		}
		writeProtoError(w, err)
		return
	}
	// 刷新路径也要查 disabled。不查的话，一个被禁用的用户
	// 只要手里还有刷新令牌，就能一直续命 access token ——
	// 「禁用」会变成一个最长 30 天（RefreshTokenTTL）才生效的操作。
	if u.Disabled {
		_, _ = s.store.RevokeUserRefreshTokens(ctx, u.ID, now)
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "账号已被禁用")
		return
	}

	// 轮换：同一 family 里发新令牌。
	if err := s.issueRefreshToken(w, r, u, rec.FamilyID); err != nil {
		writeProtoError(w, err)
		return
	}

	s.auditRequest(r, auditEntry{
		UserID: u.ID, Action: auditTokenRefresh, Result: auditResultOK,
		Meta: map[string]any{"family_id": rec.FamilyID},
	})

	token, _, exp, err := s.tokens.Issue(u.ID, now)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(exp.Sub(now).Seconds()),
	})
}

// revokeFamilyAndAudit 吊销整族刷新令牌并记审计。
func (s *Server) revokeFamilyAndAudit(ctx context.Context, r *http.Request, rec *storage.RefreshToken, reason string) {
	n, err := s.store.RevokeRefreshTokenFamily(ctx, rec.FamilyID, s.now())
	if err != nil {
		s.log.Error("吊销刷新令牌族失败", "family_id", rec.FamilyID, "err", err)
	}
	s.auditRequest(r, auditEntry{
		UserID: rec.UserID, Action: auditTokenReuse, Result: auditResultDenied,
		Meta: map[string]any{
			"family_id": rec.FamilyID,
			"reason":    reason,
			"revoked":   n,
		},
	})
	s.log.Warn("检测到刷新令牌重用，已吊销整族",
		"user_id", rec.UserID, "family_id", rec.FamilyID, "reason", reason, "revoked", n)
}

// ---------------------------------------------------------------------------
// 登出
// ---------------------------------------------------------------------------

// handleLogout 吊销当前刷新令牌所属的整族。
//
// POST /api/v1/auth/logout   （Bearer）
//
// 吊销整族而不是单个令牌：登出的语义是「结束这次登录会话」，
// 而一次登录会话对应一个 family（登录时新建，之后每次刷新都留在族内）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	now := s.now()

	// 没有 Cookie 也算登出成功。
	//
	// 登出是幂等操作：用户可能在另一个标签页已经登出过，
	// 或者手动清了 Cookie。为此报错只会让前端的登出按钮时灵时不灵。
	if plain, err := readRefreshCookie(r); err == nil {
		if rec, err := s.store.RefreshTokenByHash(r.Context(), auth.HashRefreshToken(plain)); err == nil {
			// 只吊销自己的令牌 —— 防止拿别人的 Cookie 来踢人。
			if rec.UserID == userID {
				if _, err := s.store.RevokeRefreshTokenFamily(r.Context(), rec.FamilyID, now); err != nil {
					s.log.Warn("登出时吊销令牌族失败", "family_id", rec.FamilyID, "err", err)
				}
			}
		}
	}

	s.clearRefreshCookie(w)
	s.auditRequest(r, auditEntry{UserID: userID, Action: auditLogout, Result: auditResultOK})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 改密码
// ---------------------------------------------------------------------------

// handleChangePassword 改密码并吊销所有刷新令牌。
//
// POST /api/v1/auth/password   （Bearer）
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	req, ok := decodeJSON[changePasswordRequest](w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		writeProtoError(w, err)
		return
	}

	// ★ 必须验旧密码。
	//
	// 只靠 Bearer 是不够的：access token 活 15 分钟，一台没锁屏的电脑
	// 被同事顺手改掉密码，用户就永久失去账号了（改密码会吊销所有会话）。
	// 要求旧密码把「临时接触设备」这个威胁挡在外面。
	match, err := auth.VerifyPassword(u.PasswordHash, req.CurrentPassword)
	if err != nil || !match {
		s.auditRequest(r, auditEntry{
			UserID: userID, Action: auditPasswordChange, Result: auditResultDenied,
			Meta: map[string]any{"reason": "bad_current_password"},
		})
		writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "当前密码不正确")
		return
	}

	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), err.Error())
		return
	}
	if req.NewPassword == req.CurrentPassword {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), "新密码不能与当前密码相同")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeProtoError(w, err)
		return
	}

	now := s.now()
	if err := s.store.UpdateUserPassword(ctx, userID, hash, now); err != nil {
		writeProtoError(w, err)
		return
	}

	// 吊销所有刷新令牌（§14.2）。
	//
	// 这一步同时把**当前这次会话**也踢掉了 —— 这是刻意的：
	// 改密码最常见的动机就是「怀疑账号被别人用了」，
	// 此时保留任何既有会话都违背用户的意图。
	n, err := s.store.RevokeUserRefreshTokens(ctx, userID, now)
	if err != nil {
		s.log.Error("改密码后吊销刷新令牌失败", "user_id", userID, "err", err)
	}

	s.clearRefreshCookie(w)
	s.auditRequest(r, auditEntry{
		UserID: userID, Action: auditPasswordChange, Result: auditResultOK,
		Meta: map[string]any{"revoked_tokens": n},
	})

	// 明确告诉前端「请重新登录」，而不是让它拿着刚被吊销的凭证继续试。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"reauth_required": true,
	})
}

// ---------------------------------------------------------------------------
// 当前用户
// ---------------------------------------------------------------------------

// handleMe 返回当前用户。
//
// GET /api/v1/me   （Bearer）
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())

	// 中间件已经查过一次库了，但那是为了确认用户有效；
	// 这里再查一次是为了拿到 email/role。两次查询之间理论上用户可能被删，
	// 那种情况下如实返回 404 比返回一个过期快照更正确。
	u, err := s.store.UserByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, string(protocol.CodeUnauthenticated), "账号不存在")
			return
		}
		writeProtoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meResponse{ID: u.ID, Email: u.Email, Role: u.Role})
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// issueSession 是「登录成功」的公共尾巴：发 access token + 建刷新令牌族 + 写 Cookie。
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, u *storage.User) error {
	now := s.now()

	// 新建一个 family：这是「一次登录会话」的标识，
	// 后续每次刷新都留在族内，族被吊销 = 这次登录会话结束。
	if err := s.issueRefreshToken(w, r, u, uuid.NewString()); err != nil {
		return err
	}

	token, _, exp, err := s.tokens.Issue(u.ID, now)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(exp.Sub(now).Seconds()),
	})
	return nil
}

// issueRefreshToken 生成一个刷新令牌、落库、写进 Cookie。
func (s *Server) issueRefreshToken(w http.ResponseWriter, r *http.Request, u *storage.User, familyID string) error {
	plain, hash, err := auth.NewRefreshToken()
	if err != nil {
		return err
	}

	now := s.now()
	expires := now.Add(s.cfg.RefreshTokenTTL)

	rec := &storage.RefreshToken{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		TokenHash: hash,
		FamilyID:  familyID,
		ExpiresAt: expires,
		UserAgent: truncate(r.UserAgent(), maxUserAgentLen),
		IP:        s.clientIP(r),
		CreatedAt: now,
	}
	if err := s.store.CreateRefreshToken(r.Context(), rec); err != nil {
		return err
	}

	s.setRefreshCookie(w, plain, expires)
	return nil
}

// setRefreshCookie 写刷新令牌 Cookie。
func (s *Server) setRefreshCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  refreshCookieName,
		Value: token,
		Path:  refreshCookiePath,
		// HttpOnly：JS 读不到（防 XSS 偷令牌）。
		HttpOnly: true,
		// Secure：只在 HTTPS 上发送。由对外协议决定而不是本地监听方式 ——
		// 反代终止 TLS 时本地是 http，但浏览器看到的是 https（§13.3）。
		Secure: s.cfg.SecureCookies(),
		// SameSite=Strict：跨站请求不带这个 Cookie（防 CSRF）。
		// 刷新端点是唯一用 Cookie 的地方，而它不需要被任何外部页面触发。
		SameSite: http.SameSiteStrictMode,
		// MaxAge 用相对秒数、Expires 用绝对时间，两者都给：
		// 老浏览器只认 Expires，新浏览器优先 MaxAge。
		MaxAge:  int(expires.Sub(s.now()).Seconds()),
		Expires: expires,
	})
}

// clearRefreshCookie 让浏览器丢掉刷新令牌。
func (s *Server) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteStrictMode,
		// ★ MaxAge < 0 才是「立刻删除」。
		// 只把 Value 置空、MaxAge 设 0 的话，浏览器会把它当成
		// 一个「会话 Cookie」继续留着，下次刷新请求照样会带上它。
		MaxAge:  -1,
		Expires: time.Unix(0, 0),
	})
}

// readRefreshCookie 读刷新令牌。
func readRefreshCookie(r *http.Request) (string, error) {
	c, err := r.Cookie(refreshCookieName)
	if err != nil {
		return "", err
	}
	if c.Value == "" {
		return "", http.ErrNoCookie
	}
	return c.Value, nil
}

// registerAllowed 判断注册请求是否放行。
//
// 与登录用**不同的限流器**：注册和登录是两种攻击面。
// 登录爆破针对某个已存在的账号，注册洪水针对整张 users 表 ——
// 共用一张桶表只会让两个阈值互相挤压。
func (s *Server) registerAllowed(r *http.Request) bool {
	return s.registerLimit.Allow("ip:" + s.clientIP(r))
}

// normalizeEmail 归一化邮箱。
//
// 只做 trim + 转小写：**不做** Gmail 那种「去掉点号」的规范化 ——
// 那会把 `a.b@x.com` 和 `ab@x.com` 判成同一个账号，而它们在 RFC 里是
// 两个不同地址。真要合并是产品决策，不该藏在认证层里悄悄做。
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validEmail 做一次**宽松**的邮箱格式检查。
//
// 刻意不追求 RFC 5322 的完整语法：那个正则长达几 KB 且几乎没人真的实现。
// 这里只挡住明显不是邮箱的输入（空、没有 @、有空格、超长），
// 真正的有效性验证靠「能不能收到信」—— 而 MVP 阶段连验证邮件都还没有。
func validEmail(s string) bool {
	if s == "" || len(s) > maxEmailLen {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	// 只能有一个 @，且不能有空白/控制字符。
	if strings.Count(s, "@") != 1 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7F {
			return false
		}
	}
	// 域名部分必须有点，且点不在首尾。
	domain := s[at+1:]
	return strings.Contains(domain, ".") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
