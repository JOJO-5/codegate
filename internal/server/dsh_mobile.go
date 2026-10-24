package server

import (
	"bytes"
	"embed"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed dshmobile/v1.js dshmobile/v1.css
var dshMobileAssets embed.FS

const dshMobilePrefix = "/_codegate/dsh-mobile/"

func (s *Server) serveDSHMobileAsset(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, dshMobilePrefix) {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, dshMobilePrefix)
	if s.cfg.DisableDSHMobileUI || (name != "v1.js" && name != "v1.css") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return true
	}
	content, _ := dshMobileAssets.ReadFile("dshmobile/" + name)
	mime := "text/javascript; charset=utf-8"
	if name == "v1.css" {
		mime = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
	return true
}

// Only the verified rc.2 frontend is adapted. Unknown builds get an explicit
// notice but retain their native layout; no npm files or CSP rules are changed.
func (s *Server) adaptDSHMobile(res *http.Response) error {
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	if s.cfg.DisableDSHMobileUI {
		res.Header.Set("X-CodeGate-DSH-Mobile", "disabled")
		return nil
	}
	if encoding := res.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		res.Header.Set("X-CodeGate-DSH-Mobile", "unsupported-encoding")
		return nil
	}
	// Respect restrictive policies instead of adding unsafe-inline/nonce bypasses.
	csp := res.Header.Get("Content-Security-Policy")
	if csp != "" {
		res.Header.Set("X-CodeGate-DSH-Mobile", "unsupported-csp")
		return nil
	}
	original := res.Body
	data, err := io.ReadAll(io.LimitReader(original, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(data), original), original}
		res.Header.Set("X-CodeGate-DSH-Mobile", "unsupported-size")
		return nil
	}
	_ = original.Close()
	html := string(data)
	supported := "false"
	state := "unsupported"
	if strings.Contains(html, "index-5SrrfWpU.js") && strings.Contains(html, "index-BPHePDI_.css") && strings.Contains(html, "vendor-CCJJTK99.js") {
		supported = "true"
		state = "verified-rc2"
	}
	if strings.Contains(html, "</head>") && strings.Contains(html, "</body>") && strings.Contains(html, "DeepSeek Harness") {
		html = strings.Replace(html, "</head>", `<link rel="stylesheet" href="`+dshMobilePrefix+`v1.css"></head>`, 1)
		html = strings.Replace(html, "</body>", `<script id="cg-dsh-mobile-script" src="`+dshMobilePrefix+`v1.js" data-supported="`+supported+`" defer></script></body>`, 1)
		if supported == "true" {
			html = strings.Replace(html, `content="width=device-width, initial-scale=1"`, `content="width=device-width, initial-scale=1, viewport-fit=cover"`, 1)
		}
	}
	res.Body = io.NopCloser(strings.NewReader(html))
	res.ContentLength = int64(len(html))
	res.Header.Set("Content-Length", strconv.Itoa(len(html)))
	res.Header.Del("ETag")
	res.Header.Del("Content-MD5")
	res.Header.Set("Cache-Control", "no-store")
	res.Header.Set("X-CodeGate-DSH-Mobile", state)
	return nil
}
