package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jojo/codegate/internal/protocol"
)

// ---------------------------------------------------------------------------
// 探针与版本
// ---------------------------------------------------------------------------

func TestHealthzDoesNotTouchDependencies(t *testing.T) {
	e := newTestEnv(t)

	resp := e.get(t, "/healthz", "")
	if resp.Status != http.StatusOK {
		t.Fatalf("healthz 应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	if got := resp.JSON(t)["status"]; got != "ok" {
		t.Fatalf("healthz.status = %v，期望 ok", got)
	}
}

func TestVersionReportsProtocol(t *testing.T) {
	e := newTestEnv(t)

	resp := e.get(t, "/api/v1/version", "")
	if resp.Status != http.StatusOK {
		t.Fatalf("version 应返回 200，得到 %d", resp.Status)
	}
	m := resp.JSON(t)
	// 版本端点的**核心职责**是把协议版本告诉前端（§9.4）：
	// 前端据此在建立 WS 之前就能拒绝不兼容的页面。
	// 少了这个字段，端点就退化成一个没用的「服务活着吗」。
	pv, ok := m["protocol"].(map[string]any)
	if !ok {
		t.Fatalf("version 响应缺少 protocol 对象: %s", resp.Body)
	}
	cur := protocol.Current()
	if got := pv["min"]; got != float64(cur.Min) {
		t.Errorf("protocol.min = %v，期望 %d", got, cur.Min)
	}
	if got := pv["max"]; got != float64(cur.Max) {
		t.Errorf("protocol.max = %v，期望 %d", got, cur.Max)
	}
}

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

func TestRegisterReturnsTokenAndSetsRefreshCookie(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    "jojo@example.com",
		"password": "correct-horse-battery",
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("注册应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	if tok := resp.Str(t, "access_token"); tok == "" {
		t.Fatal("注册响应没有 access_token")
	}
	if got := resp.Str(t, "token_type"); !strings.EqualFold(got, "bearer") {
		t.Fatalf("token_type = %q，期望 bearer", got)
	}

	// ★ refresh token **绝不能**出现在响应体里。
	//
	// 这是设计决策（§10.1）而不是疏漏：一旦它出现在 JSON 里，
	// 前端就一定会有人把它存进 localStorage —— 那等于把
	// 「Cookie 防 XSS」这条防线整个作废。
	if _, leaked := resp.JSON(t)["refresh_token"]; leaked {
		t.Fatalf("注册响应泄露了 refresh_token: %s", resp.Body)
	}

	// 它必须走 HttpOnly cookie。
	ck := refreshCookie(resp)
	if ck == nil {
		t.Fatalf("注册响应没有设置 %s cookie", refreshCookieName)
	}
	if !ck.HttpOnly {
		t.Error("refresh cookie 必须是 HttpOnly —— 否则 JS 能读到它，XSS 即可盗取长期凭证")
	}
	if ck.Path != refreshCookiePath {
		t.Errorf("refresh cookie 的 Path = %q，期望 %q", ck.Path, refreshCookiePath)
	}
}

func TestRegisterRejectsWeakPassword(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    "weak@example.com",
		"password": "123",
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("弱密码应返回 400，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	if code := resp.ErrorCode(t); code != string(protocol.CodeInvalidPayload) {
		t.Fatalf("错误码 = %q，期望 %q", code, protocol.CodeInvalidPayload)
	}
}

func TestRegisterRejectsMalformedEmail(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    "not-an-email",
		"password": "correct-horse-battery",
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("非法邮箱应返回 400，得到 %d", resp.Status)
	}
}

func TestRegisterNormalizesEmailCase(t *testing.T) {
	e := newTestEnv(t)

	e.signup(t, "JoJo@Example.COM", "correct-horse-battery")

	// 大小写不同必须视为同一个账号 —— 否则用户换个大小写就「登录不进去」，
	// 而他会认为是密码错了。
	resp := e.login(t, "jojo@example.com", "correct-horse-battery")
	if resp.Status != http.StatusOK {
		t.Fatalf("大小写归一化后应能登录，得到 %d（body=%s）", resp.Status, resp.Body)
	}
}

func TestRegisterDuplicateEmailConflicts(t *testing.T) {
	e := newTestEnv(t)

	e.signup(t, "dup@example.com", "correct-horse-battery")

	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    "dup@example.com",
		"password": "another-long-password",
	})
	if resp.Status != http.StatusConflict {
		t.Fatalf("重复邮箱应返回 409，得到 %d（body=%s）", resp.Status, resp.Body)
	}
}

func TestRegisterClosedWhenSignupDisabled(t *testing.T) {
	e := newTestEnv(t)

	// 第一个用户永远允许（引导逻辑）—— 否则全新部署的实例
	// 永远无法创建第一个账号。
	e.signup(t, "first@example.com", "correct-horse-battery")

	e.srv.cfg.AllowSignup = false

	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    "second@example.com",
		"password": "correct-horse-battery",
	})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("关闭注册后应返回 403，得到 %d（body=%s）", resp.Status, resp.Body)
	}
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

func TestLoginSuccess(t *testing.T) {
	e := newTestEnv(t)
	e.signup(t, "jojo@example.com", "correct-horse-battery")

	resp := e.login(t, "jojo@example.com", "correct-horse-battery")
	if resp.Status != http.StatusOK {
		t.Fatalf("登录应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	if resp.Str(t, "access_token") == "" {
		t.Fatal("登录响应没有 access_token")
	}
	if refreshCookie(resp) == nil {
		t.Fatal("登录响应没有设置 refresh cookie")
	}
}

// TestLoginFailuresAreIndistinguishable 是本包最重要的一条安全断言。
//
// 「用户不存在」与「密码错误」必须给出**完全相同**的响应。
// 区分开的话，登录接口就退化成一个邮箱枚举器：
// 攻击者用它确认哪些邮箱注册过，再去针对性爆破或钓鱼。
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newTestEnv(t)
	e.signup(t, "exists@example.com", "correct-horse-battery")

	wrongPassword := e.login(t, "exists@example.com", "wrong-password-entirely")
	noSuchUser := e.login(t, "ghost@example.com", "wrong-password-entirely")

	if wrongPassword.Status != noSuchUser.Status {
		t.Fatalf("状态码不同：密码错误=%d，用户不存在=%d —— 这足以枚举邮箱",
			wrongPassword.Status, noSuchUser.Status)
	}
	if wrongPassword.Status != http.StatusUnauthorized {
		t.Fatalf("登录失败应返回 401，得到 %d", wrongPassword.Status)
	}
	if string(wrongPassword.Body) != string(noSuchUser.Body) {
		t.Fatalf("响应体不同，可被用来枚举邮箱：\n  密码错误: %s\n  用户不存在: %s",
			wrongPassword.Body, noSuchUser.Body)
	}
}

func TestLoginRejectsDisabledAccount(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "banned@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)

	// 直接改库把账号停用。
	if err := e.store.SetUserDisabled(t.Context(), userID, true, e.clock.Now()); err != nil {
		t.Fatalf("停用账号失败: %v", err)
	}

	resp := e.login(t, "banned@example.com", "correct-horse-battery")
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("停用账号登录应返回 401，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// ★ 已有 token 必须立刻失效。
	//
	// 这是 §10.1「token 里不放权限声明」的直接收益：
	// 代价是每次请求多一次主键查询，收益是停用**立刻**生效，
	// 而不是等最长 15 分钟的 access token 过期窗口 ——
	// 在这个窗口里，一个已被停用的账号仍然能连上你的机器。
	me := e.get(t, "/api/v1/me", token)
	if me.Status != http.StatusUnauthorized {
		t.Fatalf("停用后旧 token 仍可用（status=%d）—— 停用不是立刻生效的", me.Status)
	}
}

// ---------------------------------------------------------------------------
// 当前用户
// ---------------------------------------------------------------------------

func TestMeRequiresAuth(t *testing.T) {
	e := newTestEnv(t)

	resp := e.get(t, "/api/v1/me", "")
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("无 token 访问 /me 应返回 401，得到 %d", resp.Status)
	}
	if code := resp.ErrorCode(t); code != string(protocol.CodeUnauthenticated) {
		t.Fatalf("错误码 = %q，期望 %q", code, protocol.CodeUnauthenticated)
	}
}

func TestMeRejectsGarbageToken(t *testing.T) {
	e := newTestEnv(t)

	for _, tok := range []string{"garbage", "a.b.c", "eyJhbGciOiJub25lIn0.e30."} {
		resp := e.get(t, "/api/v1/me", tok)
		if resp.Status != http.StatusUnauthorized {
			t.Errorf("token=%q 应返回 401，得到 %d", tok, resp.Status)
		}
	}
}

func TestMeDoesNotLeakPasswordHash(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	resp := e.get(t, "/api/v1/me", token)
	if resp.Status != http.StatusOK {
		t.Fatalf("/me 应返回 200，得到 %d", resp.Status)
	}

	m := resp.JSON(t)
	if _, leaked := m["password_hash"]; leaked {
		t.Fatalf("/me 泄露了 password_hash: %s", resp.Body)
	}
	if _, leaked := m["PasswordHash"]; leaked {
		t.Fatalf("/me 泄露了 PasswordHash: %s", resp.Body)
	}
	if got := m["email"]; got != "jojo@example.com" {
		t.Fatalf("email = %v，期望 jojo@example.com", got)
	}
}

// ---------------------------------------------------------------------------
// 刷新令牌轮换与重用检测
// ---------------------------------------------------------------------------

func TestRefreshRotatesToken(t *testing.T) {
	e := newTestEnv(t)
	e.signup(t, "jojo@example.com", "correct-horse-battery")

	first := e.login(t, "jojo@example.com", "correct-horse-battery")
	ck1 := refreshCookie(first)
	if ck1 == nil {
		t.Fatal("登录没有下发 refresh cookie")
	}

	second := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck1)
	if second.Status != http.StatusOK {
		t.Fatalf("刷新应返回 200，得到 %d（body=%s）", second.Status, second.Body)
	}

	ck2 := refreshCookie(second)
	if ck2 == nil {
		t.Fatal("刷新没有下发新的 refresh cookie")
	}
	if ck2.Value == ck1.Value {
		t.Fatal("刷新后 refresh token 没有轮换 —— 同一个令牌可以无限次使用，等于没有重用检测")
	}
}

// TestRefreshReuseRevokesFamily 验证 §10.1 的核心安全属性。
//
// 已用过的 refresh token 被再次使用时，**整族**令牌都必须被吊销。
//
// 为什么是「整族」而不是「只拒这一个」：重用意味着这个令牌曾被泄露
// （攻击者拿到过，或者用户的设备被人拷走了）。此时我们无法区分
// 「攻击者在用旧的」和「本人在用新的」—— 唯一安全的动作是把两边都踢下线，
// 让本人重新登录。只拒旧令牌的话，攻击者手里的那个虽然失效了，
// 但他**已经用旧的换到过新的**，而新的还在他手上，只是我们不知道。
func TestRefreshReuseRevokesFamily(t *testing.T) {
	e := newTestEnv(t)
	e.signup(t, "jojo@example.com", "correct-horse-battery")

	login := e.login(t, "jojo@example.com", "correct-horse-battery")
	ck1 := refreshCookie(login)

	// 正常轮换一次，拿到 ck2。
	rotated := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck1)
	if rotated.Status != http.StatusOK {
		t.Fatalf("第一次刷新失败: %d %s", rotated.Status, rotated.Body)
	}
	ck2 := refreshCookie(rotated)
	if ck2 == nil {
		t.Fatal("轮换没有返回新 cookie")
	}

	// ★ 重放旧令牌 —— 这就是「令牌泄露」的信号。
	replay := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck1)
	if replay.Status == http.StatusOK {
		t.Fatal("重放已用过的 refresh token 竟然成功了")
	}

	// 整族必须已吊销：**新**令牌也不能再用。
	afterRevoke := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck2)
	if afterRevoke.Status == http.StatusOK {
		t.Fatalf("重用检测没有吊销整族：新令牌仍可用（status=%d body=%s）",
			afterRevoke.Status, afterRevoke.Body)
	}
}

func TestRefreshWithoutCookieFails(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/auth/refresh", "", nil)
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("无 cookie 刷新应返回 401，得到 %d（body=%s）", resp.Status, resp.Body)
	}
}

func TestRefreshRejectsUnknownCookie(t *testing.T) {
	e := newTestEnv(t)

	resp := e.postWithCookie(t, "/api/v1/auth/refresh", "",
		&http.Cookie{Name: refreshCookieName, Value: "totally-made-up-token"})
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("伪造 cookie 刷新应返回 401，得到 %d", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// 登出
// ---------------------------------------------------------------------------

func TestLogoutRevokesRefreshToken(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	login := e.login(t, "jojo@example.com", "correct-horse-battery")
	ck := refreshCookie(login)

	resp := e.postWithCookie(t, "/api/v1/auth/logout", token, ck)
	if resp.Status != http.StatusOK && resp.Status != http.StatusNoContent {
		t.Fatalf("登出应返回 200/204，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// 登出后 refresh token 必须失效 —— 否则「登出」只是前端删了个变量，
	// 攻击者手里的 cookie 还能继续换 access token。
	after := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck)
	if after.Status == http.StatusOK {
		t.Fatal("登出后 refresh token 仍然可用")
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	e.signup(t, "jojo@example.com", "correct-horse-battery")

	login := e.login(t, "jojo@example.com", "correct-horse-battery")
	token := login.Str(t, "access_token")
	ck := refreshCookie(login)
	if ck == nil {
		t.Fatal("登录没有下发 refresh cookie")
	}

	// 幂等：第二次登出不该报错。
	// 移动端网络抖动导致的重复请求很常见，报错只会让用户看到
	// 一个莫名其妙的「登出失败」，而实际上他已经登出了。
	// 第二次请求带的 cookie 已经指向一个被吊销的令牌族 ——
	// 这条路必须走通，而不是在「找不到有效令牌」处报 500。
	for i := range 2 {
		resp := e.postWithCookie(t, "/api/v1/auth/logout", token, ck)
		if resp.Status >= 500 {
			t.Fatalf("第 %d 次登出返回 %d（body=%s）", i+1, resp.Status, resp.Body)
		}
	}
}

func TestLogoutRequiresAuth(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/auth/logout", "", nil)
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("无 token 登出应返回 401，得到 %d", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// 改密码
// ---------------------------------------------------------------------------

func TestChangePasswordRevokesSessions(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	login := e.login(t, "jojo@example.com", "correct-horse-battery")
	ck := refreshCookie(login)

	resp := e.post(t, "/api/v1/auth/password", token, map[string]string{
		"current_password": "correct-horse-battery",
		"new_password":     "brand-new-long-password",
	})
	if resp.Status != http.StatusOK && resp.Status != http.StatusNoContent {
		t.Fatalf("改密码应成功，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// 旧密码必须失效。
	if old := e.login(t, "jojo@example.com", "correct-horse-battery"); old.Status == http.StatusOK {
		t.Fatal("改密码后旧密码仍能登录")
	}
	// 新密码必须可用。
	if fresh := e.login(t, "jojo@example.com", "brand-new-long-password"); fresh.Status != http.StatusOK {
		t.Fatalf("新密码无法登录：%d（body=%s）", fresh.Status, fresh.Body)
	}

	// ★ 改密码必须吊销所有既有 refresh token。
	//
	// 用户改密码的动机里，「我怀疑账号被盗了」占很大比例。
	// 不吊销的话，攻击者手里的 refresh token 还能继续换 access token ——
	// 改密码这个动作就完全没有达到用户期望的效果。
	if after := e.postWithCookie(t, "/api/v1/auth/refresh", "", ck); after.Status == http.StatusOK {
		t.Fatal("改密码后旧 refresh token 仍然可用 —— 被盗的会话没有被踢下线")
	}
}

func TestChangePasswordRequiresCurrentPassword(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	resp := e.post(t, "/api/v1/auth/password", token, map[string]string{
		"current_password": "not-the-real-one",
		"new_password":     "brand-new-long-password",
	})
	if resp.Status != http.StatusUnauthorized && resp.Status != http.StatusForbidden {
		t.Fatalf("旧密码错误应被拒，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// 密码不能被改动。
	if still := e.login(t, "jojo@example.com", "correct-horse-battery"); still.Status != http.StatusOK {
		t.Fatal("旧密码验证失败后，密码竟然被改掉了")
	}
}

// ---------------------------------------------------------------------------
// WS 票据
// ---------------------------------------------------------------------------

func TestWSTicketRequiresAuth(t *testing.T) {
	e := newTestEnv(t)

	resp := e.post(t, "/api/v1/ws-ticket", "", nil)
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("无 token 申请票据应返回 401，得到 %d", resp.Status)
	}
}

func TestWSTicketIsSingleUse(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	resp := e.post(t, "/api/v1/ws-ticket", token, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("申请票据应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	m := resp.JSON(t)
	ticket, _ := m["ticket"].(string)
	if ticket == "" {
		t.Fatalf("响应里没有 ticket: %s", resp.Body)
	}
	if m["protocol"] == nil {
		t.Error("票据响应缺少 protocol 字段 —— 前端无法在建立 WS 前判断兼容性")
	}

	// ★ 票据用完即焚：第二次消费必须失败。
	// 这是防重放的唯一机制 —— 票据会出现在 URL 里（浏览器 WS 不支持自定义头），
	// 而 URL 会进日志、Referer、浏览器历史。
	if uid, ok := e.srv.tickets.Consume(ticket, e.clock.Now()); !ok || uid == "" {
		t.Fatal("第一次消费票据失败")
	}
	if _, ok := e.srv.tickets.Consume(ticket, e.clock.Now()); ok {
		t.Fatal("票据可以被重复消费 —— 泄露的票据可被用来抢占连接")
	}
}

func TestWSTicketExpires(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")

	e.clock.Advance(wsTicketTTL + 1e9)

	if _, ok := e.srv.tickets.Consume(ticket, e.clock.Now()); ok {
		t.Fatal("过期票据仍可用")
	}
}

// ---------------------------------------------------------------------------
// Origin 校验
// ---------------------------------------------------------------------------

func TestWSClientRejectsForeignOrigin(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowedOrigins = []string{"https://codegate.example.com"}

	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/api/v1/ws/client", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example.com")

	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// ★ WebSocket 不受同源策略保护：没有这道闸，用户访问任意恶意页面
	// 就能被它拿 cookie 连上我们的服务（CSRF over WebSocket）。
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("外域 Origin 应被拒（403），得到 %d", resp.StatusCode)
	}
}
