package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestRecoverPreservesAbortWithoutLoggingPanic(t *testing.T) {
	for _, value := range []error{http.ErrAbortHandler, fmt.Errorf("stream closed: %w", http.ErrAbortHandler)} {
		var logs bytes.Buffer
		s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
		w := httptest.NewRecorder()
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			s.withRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "data: partial\n\n")
				panic(value)
			})).ServeHTTP(w, httptest.NewRequest("GET", "/plugins/events", nil))
		}()
		if recovered != http.ErrAbortHandler || logs.Len() != 0 || w.Body.String() != "data: partial\n\n" {
			t.Fatalf("abort must reach net/http without a panic alarm or appended error; recovered=%v logs=%q body=%q", recovered, logs.String(), w.Body.String())
		}
	}
}

func TestRecoverKeepsUnexpectedPanicVisible(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
	w := httptest.NewRecorder()
	s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("unexpected fixture bug") })).ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	if w.Code != 500 || !strings.Contains(logs.String(), "HTTP handler panic") || !strings.Contains(w.Body.String(), "internal") {
		t.Fatalf("unexpected panic lost its error response or diagnostic: status=%d logs=%q body=%q", w.Code, logs.String(), w.Body.String())
	}
}

func TestRecoverDoesNotCompleteTruncatedProxyStream(t *testing.T) {
	const payload = "data: partial\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		// Drop a real chunked SSE connection without its terminating zero chunk.
		_, _ = fmt.Fprintf(writer, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(payload), payload)
		_ = writer.Flush()
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
	front := httptest.NewServer(s.withRecover(proxy))
	defer front.Close()
	response, err := front.Client().Get(front.URL + "/plugins/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if readErr == nil || string(body) != payload || logs.Len() != 0 {
		t.Fatalf("truncated stream must remain incomplete without a panic alarm: err=%v body=%q logs=%q", readErr, body, logs.String())
	}
}
