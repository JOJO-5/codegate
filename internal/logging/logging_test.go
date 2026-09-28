package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"password", "Password", "user_password", "passwd",
		"token", "access_token", "refreshToken", "X-Api-Key", "api.key",
		"Authorization", "cookie", "private_key", "privateKey",
		"client_secret", "signature", "nonce", "credential",
	}
	for _, k := range sensitive {
		if !IsSensitiveKey(k) {
			t.Errorf("%q 应被判定为敏感字段", k)
		}
	}

	notSensitive := []string{"user_id", "email", "device_name", "status", "cols", "rows"}
	for _, k := range notSensitive {
		if IsSensitiveKey(k) {
			t.Errorf("%q 不应被判定为敏感字段（会白遮掉有用信息）", k)
		}
	}
}

func TestRedactAttrTopLevel(t *testing.T) {
	got := RedactAttr(slog.String("password", "hunter2"))
	if got.Value.String() != Redacted {
		t.Errorf("敏感字段应被替换，实际: %q", got.Value.String())
	}

	// 非敏感字段必须原样保留，否则日志就废了。
	got = RedactAttr(slog.String("device_name", "JOJO-PC"))
	if got.Value.String() != "JOJO-PC" {
		t.Errorf("非敏感字段不应被改动，实际: %q", got.Value.String())
	}
}

func TestRedactAttrNestedGroup(t *testing.T) {
	// 嵌套 group 是最容易漏的场景：只看顶层 key 会完全放过里面的 token。
	attr := slog.Group("req",
		slog.String("path", "/api/v1/auth/login"),
		slog.String("token", "super-secret"),
		slog.Group("headers",
			slog.String("Authorization", "Bearer abc.def.ghi"),
			slog.String("Accept", "application/json"),
		),
	)

	got := RedactAttr(attr)
	out := renderAttr(got)

	if strings.Contains(out, "super-secret") {
		t.Errorf("嵌套 token 泄露了:\n%s", out)
	}
	if strings.Contains(out, "abc.def.ghi") {
		t.Errorf("嵌套 Authorization 泄露了:\n%s", out)
	}
	// 非敏感字段仍在。
	if !strings.Contains(out, "/api/v1/auth/login") || !strings.Contains(out, "application/json") {
		t.Errorf("非敏感字段被误删:\n%s", out)
	}
}

func TestRedactAttrMapValue(t *testing.T) {
	// `log.Info("req", "body", map[string]any{...})` 这类调用走 KindAny，
	// 不递归的话脱敏等于没做。
	attr := slog.Any("body", map[string]any{
		"email":    "jojo@x.com",
		"password": "hunter2",
		"nested":   map[string]any{"refresh_token": "rt-123"},
	})

	out := renderAttr(RedactAttr(attr))
	if strings.Contains(out, "hunter2") {
		t.Errorf("map 里的密码泄露了:\n%s", out)
	}
	if strings.Contains(out, "rt-123") {
		t.Errorf("嵌套 map 里的 token 泄露了:\n%s", out)
	}
	if !strings.Contains(out, "jojo@x.com") {
		t.Errorf("非敏感值被误删:\n%s", out)
	}
}

func TestRedactURL(t *testing.T) {
	u, err := url.Parse("https://cg.example.com/api/v1/ws/client?ticket=abc123&foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	got := RedactURL(u)

	if strings.Contains(got, "abc123") {
		t.Errorf("一次性票据不应出现在日志里: %s", got)
	}
	if !strings.Contains(got, "foo=bar") {
		t.Errorf("非敏感参数应保留: %s", got)
	}

	// 没有 query 时原样返回。
	u2, _ := url.Parse("https://cg.example.com/healthz")
	if RedactURL(u2) != "https://cg.example.com/healthz" {
		t.Errorf("无 query 时不应改动: %s", RedactURL(u2))
	}
	if RedactURL(nil) != "" {
		t.Error("nil URL 应返回空串")
	}
}

// TestHandlerEndToEnd 是最重要的一个：验证脱敏在**真实的 handler 链**上生效。
// 单独测 RedactAttr 只能证明函数对，证明不了 logger 真的挂了它。
func TestHandlerEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: "debug", Out: &buf})

	// 故意用最容易泄露的写法：整个结构体丢进日志。
	logger.Info("登录请求",
		slog.String("email", "jojo@x.com"),
		slog.Any("body", map[string]any{
			"password":      "hunter2",
			"refresh_token": "rt-abc-123",
		}),
		slog.Group("headers", slog.String("Authorization", "Bearer leaked.jwt.here")),
	)

	out := buf.String()
	for _, secret := range []string{"hunter2", "rt-abc-123", "leaked.jwt.here"} {
		if strings.Contains(out, secret) {
			t.Errorf("★ 日志里出现敏感值 %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "jojo@x.com") {
		t.Errorf("非敏感字段应当保留:\n%s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("应当出现脱敏占位符:\n%s", out)
	}
}

// TestHandlerOutputsValidJSON 确认脱敏后的记录仍然是合法 JSON ——
// 重组 record 时很容易把结构弄坏，那样日志采集直接就挂了。
func TestHandlerOutputsValidJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: "info", Out: &buf})

	logger.Info("test", slog.String("token", "x"), slog.Int("n", 1))

	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, buf.String())
	}
	if decoded["msg"] != "test" {
		t.Errorf("msg 字段丢失: %v", decoded["msg"])
	}
	if decoded["token"] != Redacted {
		t.Errorf("token 未被脱敏: %v", decoded["token"])
	}
}

func TestWithAttrsRedacts(t *testing.T) {
	var buf bytes.Buffer
	// WithAttrs 路径同样要脱敏 —— 很容易只处理 Handle 而漏掉它。
	logger := New(Options{Level: "info", Out: &buf}).With(slog.String("api_key", "k-123"))

	logger.Info("调用")

	if strings.Contains(buf.String(), "k-123") {
		t.Errorf("WithAttrs 里的敏感值泄露了:\n%s", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo, // 兜底
		"乱写":    slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// renderAttr 把 attr 渲染成字符串，供断言使用。
func renderAttr(a slog.Attr) string {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "x", 0)
	rec.AddAttrs(a)
	_ = h.Handle(context.Background(), rec)
	return buf.String()
}
