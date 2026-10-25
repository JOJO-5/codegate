package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/config"
)

const mobileFixtureHTML = `<!doctype html><html><head><title>DeepSeek Harness</title><meta name="viewport" content="width=device-width, initial-scale=1"><script src="/assets/index-5SrrfWpU.js"></script><link href="/assets/index-BPHePDI_.css"><script src="/assets/vendor-CCJJTK99.js"></script></head><body><div id="root"></div></body></html>`

func TestDSHMobileCompatibilityIsPinnedAndReversible(t *testing.T) {
	s := &Server{cfg: config.Defaults()}
	for _, tc := range []struct {
		name, html, encoding string
		disabled             bool
		supported            string
	}{{"verified", mobileFixtureHTML, "", false, "verified-rc2"}, {"unknown", strings.ReplaceAll(mobileFixtureHTML, "index-5SrrfWpU", "index-new"), "", false, "unsupported"}, {"compressed", mobileFixtureHTML, "gzip", false, "unsupported-encoding"}, {"disabled", mobileFixtureHTML, "", true, "disabled"}} {
		t.Run(tc.name, func(t *testing.T) {
			s.cfg.DisableDSHMobileUI = tc.disabled
			r := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.html)), ContentLength: int64(len(tc.html))}
			r.Header.Set("Content-Type", "text/html")
			r.Header.Set("Content-Encoding", tc.encoding)
			r.Header.Set("ETag", "old-body-tag")
			if err := s.adaptDSHMobile(r); err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("X-CodeGate-DSH-Mobile") != tc.supported {
				t.Fatalf("wrong compatibility feedback: %s", r.Header.Get("X-CodeGate-DSH-Mobile"))
			}
			if tc.disabled || tc.encoding != "" {
				if string(body) != tc.html {
					t.Fatal("disabled or compressed upstream was modified")
				}
				return
			}
			if !strings.Contains(string(body), "/_codegate/dsh-mobile/v1.js") || !strings.Contains(string(body), "data-supported=") {
				t.Fatal("mobile bootstrap or explicit compatibility state missing")
			}
			if r.Header.Get("ETag") != "" || r.ContentLength != int64(len(body)) {
				t.Fatal("changed HTML retained invalid response validators")
			}
		})
	}
}

func TestDSHMobileDoesNotWeakenCSPOrCorruptNonHTML(t *testing.T) {
	s := &Server{cfg: config.Defaults()}
	s.cfg.DisableDSHMobileUI = false
	r := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(mobileFixtureHTML)), StatusCode: 200}
	r.Header.Set("Content-Type", "text/html")
	r.Header.Set("Content-Security-Policy", "default-src 'none'")
	if err := s.adaptDSHMobile(r); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	if string(body) != mobileFixtureHTML || r.Header.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatal("strict upstream CSP was weakened")
	}
	r = &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":"<body>"}`)), StatusCode: 200}
	r.Header.Set("Content-Type", "application/json")
	if err := s.adaptDSHMobile(r); err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(r.Body)
	if string(body) != `{"data":"<body>"}` {
		t.Fatal("API data modified")
	}
}

func TestDSHMobileAssetsRequireNormalProxyAuthorization(t *testing.T) {
	e, _, id, ac := dshOperationFixture(t)
	e.srv.cfg.DisableDSHMobileUI = false
	e.srv.web.simpleDevice = id
	for _, name := range []string{"v1.js", "v1.css"} {
		r := httptest.NewRequest("GET", "https://codegate.example.test:8443/"+strings.TrimPrefix(dshMobilePrefix, "/")+name, nil)
		w := httptest.NewRecorder()
		e.srv.serveDSH(w, r)
		if w.Code != 401 {
			t.Fatal("mobile assets bypassed proxy authorization")
		}
		e.srv.web.sessions["valid"] = webGrant{deviceID: id, userID: ac.UserID, expires: e.clock.Now().Add(time.Hour)}
		r.AddCookie(&http.Cookie{Name: webCookieName, Value: "valid"})
		w = httptest.NewRecorder()
		e.srv.serveDSH(w, r)
		if w.Code != 200 || len(w.Body.Bytes()) < 100 {
			t.Fatal("authorized mobile asset unavailable")
		}
	}
}

func TestDSHMobileOversizedHTMLRemainsReadable(t *testing.T) {
	s := &Server{cfg: config.Defaults()}
	s.cfg.DisableDSHMobileUI = false
	html := strings.Repeat("a", (2<<20)+64)
	r := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(html)), ContentLength: int64(len(html))}
	r.Header.Set("Content-Type", "text/html")
	r.Header.Set("ETag", "original")
	if err := s.adaptDSHMobile(r); err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || string(body) != html || r.Header.Get("ETag") != "original" || r.Header.Get("X-CodeGate-DSH-Mobile") != "unsupported-size" {
		t.Fatal("size limit corrupted or misreported the upstream document")
	}
}
