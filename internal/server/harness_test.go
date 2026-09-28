package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/config"
	"github.com/jojo/codegate/internal/storage"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// quietLogger 丢弃所有日志。
//
// 测试里用真实 logger 会让 `go test` 的输出被几百行访问日志淹没，
// 而真正的失败信息（t.Errorf 那几行）反而要翻很久才找到。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testClock 是一个可手动推进的时钟。
//
// 用它而不是直接 time.Now()：限流器、票据 TTL 全都依赖时间，
// 而真实时间让这些测试要么必须 sleep（慢且不稳），要么干脆测不了
// 「过期之后会怎样」这条最重要的分支。
//
// ★ 但**起点必须锚定在真实的 now**，不能用一个写死的日期。
//
// internal/auth 的 JWT 校验用的是真实 time.Now()（那是正确的 ——
// 生产代码不该为了测试去注入时钟）。如果这里把起点定在 2026-01-01，
// 那么所有 access token 在签发的瞬间就已经过期，测试会全线报
// 「access token 已过期」，而原因和被测逻辑毫无关系。
//
// 截断到秒是为了让 RFC3339 序列化往返不产生亚秒差异。
type testClock struct {
	t time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Now().UTC().Truncate(time.Second)}
}

func (c *testClock) Now() time.Time { return c.t }

// Advance 推进时钟。★ 必须在测试的 goroutine 里调用 ——
// 它不加锁，因为加锁会掩盖「测试自己把时钟并发推进了」这个 bug。
func (c *testClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// testEnv 是一整套跑起来的服务端。
type testEnv struct {
	srv   *Server
	http  *httptest.Server
	store storage.Store
	clock *testClock
}

// newTestEnv 起一套真实 SQLite + 真实 HTTP 栈的服务端。
//
// ★ 刻意**不写 fake store**。
//
// Store 接口有 30 多个方法，写一个「恰好满足当前测试」的假实现，
// 等于把测试和实现绑死：实现改了接口，假实现跟着改，
// 但**真正的 SQL 一行都没被验证过**。R9 的锁行为、ErrConflict 的
// 原子性、级联删除 —— 这些全都只存在于真实 SQL 里。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()

	store, err := storage.OpenSQLite(ctx, storage.SQLiteOptions{
		Path:   filepath.Join(t.TempDir(), "codegate-test.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	cfg := config.Defaults()
	cfg.DBPath = "test"
	// 32 字节的固定 secret：Validate 要求至少 32，而随机生成会让
	// 「token 在两个 testEnv 之间不可互换」这件事变成偶然而非必然。
	cfg.JWTSecret = []byte("0123456789abcdef0123456789abcdef")
	cfg.AllowSignup = true
	// 测试里不限流：限流是独立的一组测试对象，
	// 让它在其他测试里生效只会制造「跑到第 7 个用例突然 429」的噪音。
	cfg.AllowedOrigins = nil

	clock := newTestClock()

	srv, err := New(Options{
		Config: cfg,
		Logger: quietLogger(),
		Store:  store,
		Now:    clock.Now,
	})
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testEnv{srv: srv, http: ts, store: store, clock: clock}
}

// ---------------------------------------------------------------------------
// HTTP 请求辅助
// ---------------------------------------------------------------------------

// apiResponse 是一次 API 调用的结果。
type apiResponse struct {
	Status  int
	Body    []byte
	Cookies []*http.Cookie
	Header  http.Header
}

// JSON 把响应体解析成 map。
func (r *apiResponse) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if len(r.Body) == 0 {
		return m
	}
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n原始内容: %s", err, r.Body)
	}
	return m
}

// Str 从解析后的 JSON 里取字符串字段。
func (r *apiResponse) Str(t *testing.T, key string) string {
	t.Helper()
	m := r.JSON(t)
	v, ok := m[key]
	if !ok {
		t.Fatalf("响应里没有字段 %q；完整内容: %s", key, r.Body)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("字段 %q 不是字符串，而是 %T: %v", key, v, v)
	}
	return s
}

// ErrorCode 取统一错误体里的 error.code。
func (r *apiResponse) ErrorCode(t *testing.T) string {
	t.Helper()
	m := r.JSON(t)
	inner, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 error 对象；完整内容: %s", r.Body)
	}
	code, _ := inner["code"].(string)
	return code
}

// request 发一次请求。token 为空时不带 Authorization 头。
func (e *testEnv) request(t *testing.T, method, path, token string, body any) *apiResponse {
	t.Helper()

	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rdr = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, e.http.URL+path, rdr)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return &apiResponse{
		Status:  resp.StatusCode,
		Body:    data,
		Cookies: resp.Cookies(),
		Header:  resp.Header,
	}
}

// get / post / patch / del 是 request 的简写。
func (e *testEnv) get(t *testing.T, path, token string) *apiResponse {
	t.Helper()
	return e.request(t, http.MethodGet, path, token, nil)
}

func (e *testEnv) post(t *testing.T, path, token string, body any) *apiResponse {
	t.Helper()
	return e.request(t, http.MethodPost, path, token, body)
}

func (e *testEnv) patch(t *testing.T, path, token string, body any) *apiResponse {
	t.Helper()
	return e.request(t, http.MethodPatch, path, token, body)
}

func (e *testEnv) del(t *testing.T, path, token string) *apiResponse {
	t.Helper()
	return e.request(t, http.MethodDelete, path, token, nil)
}

// postWithCookie 发一个带 Cookie（可带 Bearer token）的 POST。
//
// refresh token 只走 cookie（不走 body、不走 Authorization），
// 所以刷新/登出这两条路径没法用普通的 post 来测。
// ck 为 nil 时不带 Cookie —— 用来验证「没有 cookie 就该被拒」。
func (e *testEnv) postWithCookie(t *testing.T, path, token string, ck *http.Cookie) *apiResponse {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, e.http.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return &apiResponse{
		Status:  resp.StatusCode,
		Body:    data,
		Cookies: resp.Cookies(),
		Header:  resp.Header,
	}
}

// ---------------------------------------------------------------------------
// 业务辅助
// ---------------------------------------------------------------------------

// signup 注册一个账号并返回 access token。
func (e *testEnv) signup(t *testing.T, email, password string) string {
	t.Helper()
	resp := e.post(t, "/api/v1/auth/register", "", map[string]string{
		"email":    email,
		"password": password,
	})
	if resp.Status != http.StatusOK && resp.Status != http.StatusCreated {
		t.Fatalf("注册 %s 失败: status=%d body=%s", email, resp.Status, resp.Body)
	}
	return resp.Str(t, "access_token")
}

// login 登录并返回完整的 token 响应。
func (e *testEnv) login(t *testing.T, email, password string) *apiResponse {
	t.Helper()
	return e.post(t, "/api/v1/auth/login", "", map[string]string{
		"email":    email,
		"password": password,
	})
}

// refreshCookie 从响应里取出 refresh cookie。
func refreshCookie(resp *apiResponse) *http.Cookie {
	for _, c := range resp.Cookies {
		if c.Name == refreshCookieName {
			return c
		}
	}
	return nil
}

// meUserID 用 /me 拿当前 token 对应的用户 ID。
func (e *testEnv) meUserID(t *testing.T, token string) string {
	t.Helper()
	resp := e.get(t, "/api/v1/me", token)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /me 失败: status=%d body=%s", resp.Status, resp.Body)
	}
	return resp.Str(t, "id")
}

// seedDevice 直接往库里塞一台已绑定的设备。
//
// 走 SQL 而不是走配对流程：配对流程有自己的测试，
// 而这里需要的是「已经有一台设备」这个前置状态。
func (e *testEnv) seedDevice(t *testing.T, userID, name string) string {
	t.Helper()
	now := e.clock.Now()
	deviceID := "dev-" + name
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i + len(name))
	}

	if err := e.store.CreateDevice(context.Background(), &storage.Device{
		ID:        deviceID,
		Name:      name,
		Platform:  "windows",
		Arch:      "amd64",
		PublicKey: pub,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	if err := e.store.BindDevice(context.Background(), deviceID, userID, now); err != nil {
		t.Fatalf("绑定设备失败: %v", err)
	}
	return deviceID
}
