package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// ---------------------------------------------------------------------------
// 设备列表 / 详情
// ---------------------------------------------------------------------------

func TestDeviceListIsScopedToOwner(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	userA := e.meUserID(t, tokenA)
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")
	userB := e.meUserID(t, tokenB)

	e.seedDevice(t, userA, "alice-pc")
	e.seedDevice(t, userB, "bob-pc")

	devicesOf := func(token string) []any {
		resp := e.get(t, "/api/v1/devices", token)
		if resp.Status != http.StatusOK {
			t.Fatalf("GET /devices 失败: %d %s", resp.Status, resp.Body)
		}
		items, ok := resp.JSON(t)["devices"].([]any)
		if !ok {
			t.Fatalf("响应缺少 devices 数组: %s", resp.Body)
		}
		return items
	}

	a := devicesOf(tokenA)
	if len(a) != 1 {
		t.Fatalf("alice 应有 1 台设备，得到 %d 台", len(a))
	}
	if name := a[0].(map[string]any)["name"]; name != "alice-pc" {
		t.Fatalf("alice 看到了错误的设备: %v", name)
	}

	b := devicesOf(tokenB)
	if len(b) != 1 {
		t.Fatalf("bob 应有 1 台设备，得到 %d 台", len(b))
	}
	if name := b[0].(map[string]any)["name"]; name != "bob-pc" {
		t.Fatalf("bob 看到了错误的设备: %v", name)
	}
}

func TestDeviceListEmptyReturnsArrayNotNull(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	resp := e.get(t, "/api/v1/devices", token)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /devices 失败: %d", resp.Status)
	}
	// ★ 必须是 `[]` 而不是 `null`。
	//
	// Go 的 nil slice 会序列化成 null，前端得为它单独写一条分支 ——
	// 而「新用户还没有设备」恰恰是最常见的首屏状态，
	// 让最常见的状态走一条特殊分支是很糟的取舍。
	if _, ok := resp.JSON(t)["devices"].([]any); !ok {
		t.Fatalf("空列表必须是数组而不是 null: %s", resp.Body)
	}
}

// TestDeviceIDOR 是 S1：**跨账号访问必须返回 404，而不是 403**。
//
// 为什么是 404 而不是 403：
//
//	403 的意思是「这个东西存在，但你不能看」—— 这本身就泄露了
//	「这个 ID 对应一台真实设备」。攻击者可以用它做设备 ID 枚举，
//	确认哪些 ID 有效，再配合其他漏洞。404 把「不存在」和「无权」
//	压成同一个响应，攻击者从响应上分不出任何东西。
//
// 这是本包最重要的一组断言 —— IDOR 是最常见、也最容易被漏掉的
// Web 漏洞，而它的成因永远是「某一条路径忘了校验归属」。
func TestDeviceIDOR(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	userA := e.meUserID(t, tokenA)
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")

	victim := e.seedDevice(t, userA, "alice-secret-pc")

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"读详情", http.MethodGet, "/api/v1/devices/" + victim, nil},
		{"改名", http.MethodPatch, "/api/v1/devices/" + victim, map[string]string{"name": "pwned"}},
		{"删除", http.MethodDelete, "/api/v1/devices/" + victim, nil},
		{"读会话", http.MethodGet, "/api/v1/devices/" + victim + "/sessions", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.request(t, tc.method, tc.path, tokenB, tc.body)

			if resp.Status != http.StatusNotFound {
				t.Fatalf("跨账号%s应返回 404（不泄露存在性），得到 %d（body=%s）",
					tc.name, resp.Status, resp.Body)
			}
			// 错误信息里也不能带设备名、平台等任何细节。
			if body := string(resp.Body); containsAny(body, "alice-secret-pc", "windows", "amd64") {
				t.Fatalf("404 响应泄露了设备细节: %s", body)
			}
		})
	}

	// ★ 攻击之后必须**什么都没变**。
	//
	// 只断言状态码是不够的：一个 handler 完全可能先执行了删除、
	// 再因为某个后续检查失败而返回 404 —— 那样测试会通过，
	// 而数据已经被破坏了。
	resp := e.get(t, "/api/v1/devices/"+victim, tokenA)
	if resp.Status != http.StatusOK {
		t.Fatalf("受害者自己的设备读不到了（status=%d）—— 说明它已被越权操作破坏", resp.Status)
	}
	if name := resp.Str(t, "name"); name != "alice-secret-pc" {
		t.Fatalf("设备名被越权改成了 %q", name)
	}
}

// TestDeviceNotFoundIsIndistinguishableFromForbidden 验证
// 「设备不存在」与「设备属于别人」返回**逐字节相同**的响应。
//
// 如果两者有任何差别（状态码、错误码、message、长度），
// 攻击者就有了一个设备 ID 存在性预言机。
func TestDeviceNotFoundIsIndistinguishableFromForbidden(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	userA := e.meUserID(t, tokenA)
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")

	owned := e.seedDevice(t, userA, "alice-pc")
	missing := "dev-does-not-exist-" + uuid.NewString()

	foreign := e.get(t, "/api/v1/devices/"+owned, tokenB)
	absent := e.get(t, "/api/v1/devices/"+missing, tokenB)

	if foreign.Status != absent.Status {
		t.Fatalf("状态码不同：别人的设备=%d，不存在的设备=%d —— 这足以枚举设备 ID",
			foreign.Status, absent.Status)
	}
	if string(foreign.Body) != string(absent.Body) {
		t.Fatalf("响应体不同，可被用来判断设备是否存在：\n  别人的设备: %s\n  不存在的设备: %s",
			foreign.Body, absent.Body)
	}
}

// TestDeviceEndpointsAgreeOnForeignDevice 是上一条的**跨端点**版本。
//
// 这条测试来自一次真实发现：`/devices/{id}` 走 device.Service 返回 404，
// 而 `/devices/{id}/sessions` 走 authorizeDevice 曾返回 403 ——
// 同一台别人的设备，两个端点给出不同的状态码，攻击者只要换一个端点
// 就能把「存在性」读出来。单看任何一个端点都发现不了。
//
// 所以这里的断言是：**同一台别人的设备，所有设备端点必须给出
// 完全一致的状态码与响应体**。新增设备端点时，这条测试会自动覆盖到它。
func TestDeviceEndpointsAgreeOnForeignDevice(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	userA := e.meUserID(t, tokenA)
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")

	owned := e.seedDevice(t, userA, "alice-pc")
	missing := "dev-ghost-" + uuid.NewString()

	// 每个端点都必须是「方法 + 路径模板」，用两个不同的 id 各请求一次。
	endpoints := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"详情", http.MethodGet, "/api/v1/devices/%s", nil},
		{"会话", http.MethodGet, "/api/v1/devices/%s/sessions", nil},
		{"改名", http.MethodPatch, "/api/v1/devices/%s", map[string]string{"name": "x"}},
		{"删除", http.MethodDelete, "/api/v1/devices/%s", nil},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			foreign := e.request(t, ep.method, fmt.Sprintf(ep.path, owned), tokenB, ep.body)
			absent := e.request(t, ep.method, fmt.Sprintf(ep.path, missing), tokenB, ep.body)

			if foreign.Status != http.StatusNotFound {
				t.Fatalf("别人的设备应返回 404，得到 %d（body=%s）", foreign.Status, foreign.Body)
			}
			if foreign.Status != absent.Status {
				t.Fatalf("%s：别人的设备=%d，不存在的设备=%d —— 跨存在性可区分",
					ep.name, foreign.Status, absent.Status)
			}
			if string(foreign.Body) != string(absent.Body) {
				t.Fatalf("%s：响应体不同，可枚举设备 ID：\n  别人的设备: %s\n  不存在的设备: %s",
					ep.name, foreign.Body, absent.Body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 改名 / 删除
// ---------------------------------------------------------------------------

func TestDeviceRename(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID := e.seedDevice(t, userID, "old-name")

	resp := e.patch(t, "/api/v1/devices/"+deviceID, token, map[string]string{"name": "书房主机"})
	if resp.Status != http.StatusOK {
		t.Fatalf("改名应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	if got := resp.Str(t, "name"); got != "书房主机" {
		t.Fatalf("改名后 name = %q，期望 书房主机", got)
	}

	// 改名要落库，不能只是响应里好看。
	again := e.get(t, "/api/v1/devices/"+deviceID, token)
	if got := again.Str(t, "name"); got != "书房主机" {
		t.Fatalf("改名没有持久化：再查得到 %q", got)
	}
}

func TestDeviceRenameRejectsEmptyName(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID := e.seedDevice(t, userID, "old-name")

	resp := e.patch(t, "/api/v1/devices/"+deviceID, token, map[string]string{"name": "   "})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("空名字应返回 400，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// 名字不能被改坏。
	again := e.get(t, "/api/v1/devices/"+deviceID, token)
	if got := again.Str(t, "name"); got != "old-name" {
		t.Fatalf("非法改名竟然生效了：name = %q", got)
	}
}

func TestDeviceDelete(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID := e.seedDevice(t, userID, "doomed-pc")

	resp := e.del(t, "/api/v1/devices/"+deviceID, token)
	if resp.Status != http.StatusNoContent && resp.Status != http.StatusOK {
		t.Fatalf("删除应返回 204/200，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	if after := e.get(t, "/api/v1/devices/"+deviceID, token); after.Status != http.StatusNotFound {
		t.Fatalf("删除后仍能读到设备（status=%d）", after.Status)
	}
	if list := e.get(t, "/api/v1/devices", token); len(list.JSON(t)["devices"].([]any)) != 0 {
		t.Fatalf("删除后设备仍在列表里: %s", list.Body)
	}
}

// ---------------------------------------------------------------------------
// 会话缓存
// ---------------------------------------------------------------------------

func TestDeviceSessionsReturnsCache(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID := e.seedDevice(t, userID, "dev-pc")

	now := e.clock.Now()
	if err := e.store.UpsertSession(t.Context(), &storage.SessionMeta{
		ID:        "sess-1",
		DeviceID:  deviceID,
		UserID:    userID,
		Name:      "claude",
		Command:   "claude",
		Status:    "running",
		Cols:      120,
		Rows:      30,
		CreatedAt: now,
		CreatedBy: userID,
	}); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}

	resp := e.get(t, "/api/v1/devices/"+deviceID+"/sessions", token)
	if resp.Status != http.StatusOK {
		t.Fatalf("读会话缓存应返回 200，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	sessions, ok := resp.JSON(t)["sessions"].([]any)
	if !ok {
		t.Fatalf("响应缺少 sessions 数组: %s", resp.Body)
	}
	if len(sessions) != 1 {
		t.Fatalf("应有 1 个会话，得到 %d 个", len(sessions))
	}
	first := sessions[0].(map[string]any)
	if first["session_id"] != "sess-1" {
		t.Fatalf("session_id = %v，期望 sess-1", first["session_id"])
	}
	if first["device_id"] != deviceID {
		t.Fatalf("device_id = %v，期望 %s", first["device_id"], deviceID)
	}
}

// ---------------------------------------------------------------------------
// 配对（§10.3 / §10.4）
// ---------------------------------------------------------------------------

// seedPairingCode 塞一个待用的配对码，返回明文码。
func (e *testEnv) seedPairingCode(t *testing.T, deviceID, name string) string {
	t.Helper()

	plain, hash, err := auth.NewPairingCode()
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}

	pub := make([]byte, 32)
	if err := e.store.CreatePairingCode(t.Context(), &storage.PairingCode{
		ID:           uuid.NewString(),
		CodeHash:     hash,
		DeviceID:     deviceID,
		PublicKey:    pub,
		Name:         name,
		Platform:     "windows",
		Arch:         "amd64",
		AgentVersion: "0.1.0-test",
		AgentIP:      "192.168.1.50",
		ExpiresAt:    e.clock.Now().Add(e.srv.cfg.PairingCodeTTL),
		CreatedAt:    e.clock.Now(),
	}); err != nil {
		t.Fatalf("写入配对码失败: %v", err)
	}
	return plain
}

func TestPairPreviewThenConfirm(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	deviceID := "agent-" + uuid.NewString()
	code := e.seedPairingCode(t, deviceID, "家里的台式机")

	// ---- 第一步：预览 ----
	preview := e.post(t, "/api/v1/devices/pair", token, map[string]string{"code": code})
	if preview.Status != http.StatusOK {
		t.Fatalf("配对预览应返回 200，得到 %d（body=%s）", preview.Status, preview.Body)
	}
	m := preview.JSON(t)
	if m["device_id"] != deviceID {
		t.Fatalf("预览的 device_id = %v，期望 %s", m["device_id"], deviceID)
	}
	// ★ 这两条是两阶段确认的意义所在（§10.4）：
	// 用户必须能**看见自己在绑哪台机器**。没有它们，
	// 「猜中一个配对码」就等于「把别人的电脑绑到自己账号上」。
	if m["name"] != "家里的台式机" {
		t.Errorf("预览缺少设备名，用户无法判断是不是自己的机器: %s", preview.Body)
	}
	if m["agent_ip"] == nil || m["agent_ip"] == "" {
		t.Errorf("预览缺少 Agent IP，用户少了一条关键的辨识线索: %s", preview.Body)
	}

	// 预览阶段**不能**已经绑定。
	if d, err := e.store.DeviceByID(t.Context(), deviceID); err == nil && d.UserID != "" {
		t.Fatalf("预览阶段设备就已经被绑定了（user=%s）—— 猜中码即可完成绑定", d.UserID)
	}

	// ---- 第二步：确认 ----
	confirm := e.post(t, "/api/v1/devices/pair/confirm", token, map[string]string{
		"code": code,
		"name": "书房台式机",
	})
	if confirm.Status != http.StatusOK && confirm.Status != http.StatusCreated {
		t.Fatalf("配对确认应成功，得到 %d（body=%s）", confirm.Status, confirm.Body)
	}

	d, err := e.store.DeviceByID(t.Context(), deviceID)
	if err != nil {
		t.Fatalf("确认后设备记录不存在: %v", err)
	}
	if d.UserID == "" {
		t.Fatal("确认后设备仍未绑定到用户")
	}
	if d.Name != "书房台式机" {
		t.Fatalf("用户在确认页填的名字没有被采用：name = %q", d.Name)
	}

	// 设备应当出现在列表里。
	list := e.get(t, "/api/v1/devices", token)
	if n := len(list.JSON(t)["devices"].([]any)); n != 1 {
		t.Fatalf("配对后设备列表应有 1 台，得到 %d 台", n)
	}
}

func TestPairingCodeIsSingleUse(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	deviceID := "agent-" + uuid.NewString()
	code := e.seedPairingCode(t, deviceID, "pc")

	if r := e.post(t, "/api/v1/devices/pair/confirm", token,
		map[string]string{"code": code}); r.Status != http.StatusOK && r.Status != http.StatusCreated {
		t.Fatalf("第一次确认应成功，得到 %d（body=%s）", r.Status, r.Body)
	}

	// ★ 重放必须失败。
	//
	// 配对码是 8 位 Crockford Base32（约 2^40 空间），猜中的概率本来就低；
	// 但如果码可以重复使用，一个**已经泄露过的**码（比如用户截图发到群里）
	// 就能被反复用来绑定。单次使用把这个窗口压到零。
	replay := e.post(t, "/api/v1/devices/pair/confirm", token,
		map[string]string{"code": code})
	if replay.Status == http.StatusOK || replay.Status == http.StatusCreated {
		t.Fatal("配对码可以被重复使用")
	}
}

func TestPairingRejectsBadFormatWithoutDBLookup(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	// ★ 格式非法与「码不存在」必须返回**完全相同**的响应。
	//
	// 分开的话，攻击者可以先做一次格式校验，把搜索空间从
	// 「所有可能的字符串」砍到「所有格式合法的码」—— 而后者小得多。
	bad := e.post(t, "/api/v1/devices/pair", token, map[string]string{"code": "!!!"})
	absent := e.post(t, "/api/v1/devices/pair", token,
		map[string]string{"code": "ZZZZZZZZ"})

	if bad.Status != absent.Status {
		t.Fatalf("格式非法=%d，码不存在=%d —— 状态码差异泄露了格式信息",
			bad.Status, absent.Status)
	}
	if string(bad.Body) != string(absent.Body) {
		t.Fatalf("响应体不同，可被用来探测配对码格式：\n  %s\n  %s", bad.Body, absent.Body)
	}
}

func TestPairingCannotStealOthersDevice(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	userA := e.meUserID(t, tokenA)
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")

	victim := e.seedDevice(t, userA, "alice-pc")

	// Bob 拿到一个指向 Alice 设备的配对码（模拟码泄露）。
	code := e.seedPairingCode(t, victim, "alice-pc")

	preview := e.post(t, "/api/v1/devices/pair", tokenB, map[string]string{"code": code})
	if preview.Status != http.StatusConflict {
		t.Fatalf("绑定他人设备应在预览阶段就被拒（409），得到 %d（body=%s）",
			preview.Status, preview.Body)
	}
	if code := preview.ErrorCode(t); code != string(protocol.CodeForbidden) {
		t.Fatalf("错误码 = %q，期望 %q", code, protocol.CodeForbidden)
	}

	// 确认阶段同样必须拒绝 —— 不能只在预览挡、确认放行。
	confirm := e.post(t, "/api/v1/devices/pair/confirm", tokenB, map[string]string{"code": code})
	if confirm.Status < 400 {
		t.Fatalf("确认阶段竟然允许绑定他人设备（status=%d body=%s）", confirm.Status, confirm.Body)
	}

	// 归属必须没变。
	d, err := e.store.DeviceByID(t.Context(), victim)
	if err != nil {
		t.Fatalf("读设备失败: %v", err)
	}
	if d.UserID != userA {
		t.Fatalf("设备归属被改成了 %s（原属 %s）", d.UserID, userA)
	}
}

// ---------------------------------------------------------------------------
// 审计日志
// ---------------------------------------------------------------------------

func TestAuditLogsAreScopedToUser(t *testing.T) {
	e := newTestEnv(t)

	tokenA := e.signup(t, "alice@example.com", "correct-horse-battery")
	tokenB := e.signup(t, "bob@example.com", "correct-horse-battery")

	// Alice 做点会留下审计痕迹的事。
	userA := e.meUserID(t, tokenA)
	deviceID := e.seedDevice(t, userA, "alice-pc")
	e.patch(t, "/api/v1/devices/"+deviceID, tokenA, map[string]string{"name": "改名了"})

	bob := e.get(t, "/api/v1/audit", tokenB)
	if bob.Status != http.StatusOK {
		t.Fatalf("读审计应返回 200，得到 %d（body=%s）", bob.Status, bob.Body)
	}
	items, ok := bob.JSON(t)["items"].([]any)
	if !ok {
		t.Fatalf("响应缺少 items 数组: %s", bob.Body)
	}

	// Bob 不该看到 Alice 的任何记录。
	//
	// 审计日志里含设备 ID、会话 ID、IP —— 跨账号可读等于一份
	// 「别人家里有哪些机器、IP 是什么」的清单。
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["device_id"] == deviceID {
			t.Fatalf("Bob 的审计里出现了 Alice 的设备 %s: %s", deviceID, bob.Body)
		}
	}
}

func TestAuditLogsPagination(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID := e.seedDevice(t, userID, "pc")

	// 制造 3 条审计记录。
	for i := range 3 {
		resp := e.patch(t, "/api/v1/devices/"+deviceID, token,
			map[string]string{"name": "name-" + string(rune('a'+i))})
		if resp.Status != http.StatusOK {
			t.Fatalf("第 %d 次改名失败: %d %s", i, resp.Status, resp.Body)
		}
	}

	page := e.get(t, "/api/v1/audit?limit=2", token)
	if page.Status != http.StatusOK {
		t.Fatalf("读审计应返回 200，得到 %d", page.Status)
	}
	m := page.JSON(t)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("limit=2 应返回 2 条，得到 %d 条", len(items))
	}
	cursor, _ := m["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("拿满一页时应给出 next_cursor: %s", page.Body)
	}

	// 用游标翻下一页，且不能与第一页重复。
	next := e.get(t, "/api/v1/audit?limit=2&cursor="+cursor, token)
	if next.Status != http.StatusOK {
		t.Fatalf("游标翻页失败: %d（body=%s）", next.Status, next.Body)
	}
	second, _ := next.JSON(t)["items"].([]any)
	if len(second) == 0 {
		t.Fatal("游标翻页返回空 —— 还有更多记录没取到")
	}

	firstIDs := map[string]bool{}
	for _, it := range items {
		firstIDs[it.(map[string]any)["id"].(string)] = true
	}
	for _, it := range second {
		if id, _ := it.(map[string]any)["id"].(string); firstIDs[id] {
			t.Fatalf("游标翻页出现重复记录 id=%s", id)
		}
	}
}

func TestAuditLogsRejectsBadCursor(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	// 用 '@' 而不是 '%'：'%' 在 query 里是转义前缀，
	// 会让 url.Query() 静默丢掉整个参数（返回空游标 → 200），
	// 于是测试会因为一个与 base64 无关的原因通过。
	resp := e.get(t, "/api/v1/audit?cursor=@@@@not-base64@@@@", token)
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("非法游标应返回 400，得到 %d（body=%s）", resp.Status, resp.Body)
	}
	if code := resp.ErrorCode(t); code != string(protocol.CodeInvalidPayload) {
		t.Fatalf("错误码 = %q，期望 %q", code, protocol.CodeInvalidPayload)
	}
}

func TestAuditLogsClampsLimit(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	// ★ 超大 limit 必须被夹到上限。
	//
	// `?limit=1000000` 会让服务端去拼一个巨大的响应 ——
	// 这是一个纯读取的 DoS 面，而且不需要认证之外的任何东西。
	resp := e.get(t, "/api/v1/audit?limit=1000000", token)
	if resp.Status != http.StatusOK {
		t.Fatalf("超大 limit 应被夹紧而不是报错，得到 %d（body=%s）", resp.Status, resp.Body)
	}

	// 非法值同理：`?limit=abc` 更可能是前端拼错，为此 400 只会让页面白屏。
	if bad := e.get(t, "/api/v1/audit?limit=abc", token); bad.Status != http.StatusOK {
		t.Fatalf("非法 limit 应被夹紧而不是报错，得到 %d（body=%s）", bad.Status, bad.Body)
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// containsAny 判断 s 是否包含任意一个非空子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
