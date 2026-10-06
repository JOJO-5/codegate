package webui

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestGzipAssetNegotiationHeadAndRange(t *testing.T) {
	content := strings.Repeat("const hello = '中文 compressed';\n", 500)
	name := "assets/compression-fixture.js"
	fsys := fstest.MapFS{name: &fstest.MapFile{Data: []byte(content)}}
	for _, encoding := range []string{"gzip", "br, gzip;q=0.5", "*"} {
		r := httptest.NewRequest("GET", "/"+name, nil)
		r.Header.Set("Accept-Encoding", encoding)
		w := httptest.NewRecorder()
		if !serveGzipAsset(w, r, fsys, name) {
			t.Fatal("not encoded", encoding)
		}
		z, e := gzip.NewReader(w.Body)
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := io.ReadAll(z)
		z.Close()
		if e != nil || string(decoded) != content {
			t.Fatal("corrupt asset")
		}
		if w.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatal("missing cache variant")
		}
	}
	for _, encoding := range []string{"", "br", "gzip;q=0", "*;q=1,gzip;q=0", "gzip;q=oops"} {
		if acceptsGzip(encoding) {
			t.Fatal("ignored refusal", encoding)
		}
	}
	r := httptest.NewRequest("HEAD", "/"+name, nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	if !serveGzipAsset(w, r, fsys, name) || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatal("bad HEAD")
	}
	r = httptest.NewRequest("GET", "/"+name, nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Range", "bytes=0-8")
	if serveGzipAsset(httptest.NewRecorder(), r, fsys, name) {
		t.Fatal("compressed byte range")
	}
}
