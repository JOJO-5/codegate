package logging

import (
	"log/slog"
	"net/url"
	"strings"
)

// Redacted 是脱敏后替换的占位符。
//
// 刻意**不保留**原值的前缀/后缀或长度：即使只泄露 4 个字符，
// 对短 token 也可能是实质泄露。宁可在排障时多点一次调试器。
const Redacted = "[REDACTED]"

// sensitiveSubstrings 用**子串**匹配而不是精确匹配。
//
// 理由：字段命名千奇百怪（`user_password` / `X-Api-Key` / `refreshToken`），
// 精确匹配必然漏。误脱敏（把 `tokenizer` 也遮掉）的代价只是日志难读，
// 漏脱敏的代价是凭证泄露 —— 两者不对称，所以选宽的那一侧。
var sensitiveSubstrings = []string{
	"password", "passwd", "passphrase",
	"secret",
	"token",
	// ticket：WS 一次性票据（?ticket=xxx）。虽然只能用一次，
	// 但日志通常长期保留，泄露后在 30 秒有效期内仍可被用来抢占连接。
	"ticket",
	"private_key", "privatekey",
	"apikey", "api_key",
	"authorization",
	"cookie",
	"credential",
	"signature",
	"nonce",
}

// IsSensitiveKey 判断字段名是否敏感。
func IsSensitiveKey(key string) bool {
	norm := strings.ToLower(key)
	// 统一分隔符，让 `X-Api-Key` / `api.key` / `api_key` 命中同一条规则。
	norm = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(norm)
	for _, s := range sensitiveSubstrings {
		if strings.Contains(norm, s) {
			return true
		}
	}
	return false
}

// RedactAttr 对单个 slog.Attr 做脱敏，并递归处理 group 与常见容器类型。
func RedactAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}

	switch a.Value.Kind() {
	case slog.KindGroup:
		g := a.Value.Group()
		if len(g) == 0 {
			return a
		}
		out := make([]slog.Attr, len(g))
		for i, sub := range g {
			out[i] = RedactAttr(sub)
		}
		return slog.GroupAttrs(a.Key, out...)

	case slog.KindAny:
		// ★ 这里是真正的风险点：`log.Info("req", "body", reqStruct)` 这类调用
		// 会把整个结构体塞进 KindAny。不处理的话脱敏等于没做。
		return slog.Any(a.Key, redactAny(a.Value.Any()))
	}
	return a
}

// RedactAttrs 批量脱敏。
func RedactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = RedactAttr(a)
	}
	return out
}

// redactAny 尽力递归常见容器。遇到不认识的类型原样返回 ——
// 用反射遍历任意结构体风险太大（可能触发 panic 或死循环），
// 约定：需要打日志的复杂结构请先转成 map 或显式脱敏。
func redactAny(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if IsSensitiveKey(k) {
				out[k] = Redacted
				continue
			}
			out[k] = redactAny(val)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, val := range t {
			if IsSensitiveKey(k) {
				out[k] = Redacted
				continue
			}
			out[k] = val
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactAny(val)
		}
		return out
	case slog.Value:
		return RedactAttr(slog.Attr{Key: "v", Value: t}).Value.Any()
	default:
		return v
	}
}

// RedactURL 去掉 URL query 里的敏感参数，用于 HTTP 访问日志。
//
// 典型场景：`/api/v1/ws/client?ticket=<一次性票据>`。
// 这张票据虽然只能用一次，但日志往往会被长期保留，泄露后
// 在有效期内（30 秒）仍可被用来抢占连接。
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.String()
	}
	q := u.Query()
	changed := false
	for k := range q {
		if IsSensitiveKey(k) {
			q.Set(k, Redacted)
			changed = true
		}
	}
	if !changed {
		return u.String()
	}
	cp := *u
	cp.RawQuery = q.Encode()
	return cp.String()
}
