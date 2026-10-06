package webui

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Public, immutable embedded text assets are compressed once and reused. This
// avoids per-request compression work on the Server, including direct installs
// without Caddy. API responses and files are outside this handler.
var gzipAssets sync.Map

func acceptsGzip(value string) bool {
	wildcard := false
	for _, entry := range strings.Split(value, ",") {
		parts := strings.Split(entry, ";")
		name := strings.TrimSpace(strings.ToLower(parts[0]))
		q := 1.0
		for _, param := range parts[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(k, "q") {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil || n < 0 || n > 1 {
					q = 0
				} else {
					q = n
				}
			}
		}
		if name == "gzip" {
			return q > 0
		}
		if name == "*" {
			wildcard = q > 0
		}
	}
	return wildcard
}

func serveGzipAsset(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json", ".txt":
	default:
		return false
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) || r.Header.Get("Range") != "" || (r.Method != "GET" && r.Method != "HEAD") {
		return false
	}
	var compressed []byte
	if cached, ok := gzipAssets.Load(name); ok {
		compressed = cached.([]byte)
	} else {
		data, err := fs.ReadFile(fsys, name)
		if err != nil || len(data) < 512 {
			return false
		}
		var buf bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		_, _ = writer.Write(data)
		_ = writer.Close()
		if buf.Len() >= len(data) {
			return false
		}
		compressed = buf.Bytes()
		cached, _ := gzipAssets.LoadOrStore(name, compressed)
		compressed = cached.([]byte)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(compressed))
	return true
}
