package server

import (
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
)

var dshRecoveryPage = template.Must(template.New("dsh-recovery").Parse(`<!doctype html>
<html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>DSH Web · 需要重新连接</title>
<style>:root{color-scheme:light dark;font:16px/1.7 system-ui,sans-serif;color:#202938;background:#f2f4f4}body{margin:0;padding:24px;box-sizing:border-box}main{max-width:36rem;margin:10vh auto;background:#fff;border:1px solid #dce3e5;border-radius:12px;padding:24px;overflow-wrap:anywhere}h1{font-size:1.3rem}a{display:inline-flex;align-items:center;min-height:44px;padding:0 16px;background:#315d73;color:#fff;border-radius:8px;text-decoration:none}a:focus-visible{outline:3px solid #5c91ab;outline-offset:4px}p{margin:16px 0}@media(prefers-color-scheme:dark){:root{color:#e2e8ee;background:#151c24}main{background:#202b36;border-color:#405263}}</style>
<main><p>CodeGate · DSH Web</p><h1>需要重新连接</h1><p role="status">{{.Reason}}</p><p>这不表示原对话已删除。请返回 CodeGate 核对设备和运行状态，再选择打开 DSH；重新连接不会自动重启已有实例。</p><a href="{{.URL}}" rel="noreferrer">返回 CodeGate 重新连接</a></main></html>`))

// Only document navigations receive HTML; APIs and WebSocket handshakes keep
// their original HTTP failure semantics. No token or request URL is reflected.
func (s *Server) dshFailure(w http.ResponseWriter, r *http.Request, status int, deviceID, reason string) {
	if r.Method != http.MethodGet || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(r.Header.Get("Accept"), "text/html") || strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, reason, status)
		return
	}
	path := "/devices"
	if decoded, err := hex.DecodeString(deviceID); err == nil && len(decoded) == 16 {
		path += "/" + deviceID + "?dsh=reconnect#dsh-web-heading"
	}
	// BaseURL is validated configuration, never the browser-supplied Host.
	target := strings.TrimRight(s.cfg.BaseURL, "/") + path
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = dshRecoveryPage.Execute(w, struct{ Reason, URL string }{reason, target})
}
