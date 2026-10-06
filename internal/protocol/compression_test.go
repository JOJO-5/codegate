package protocol

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type countedConn struct {
	net.Conn
	written atomic.Int64
}

func (c *countedConn) Write(b []byte) (int, error) {
	n, e := c.Conn.Write(b)
	c.written.Add(int64(n))
	return n, e
}

func TestCompressionWireIntegrityAndDecodedLimit(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "deflate"}[compression], func(t *testing.T) {
			received := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := websocket.Upgrader{EnableCompression: true}
				ws, e := u.Upgrade(w, r, nil)
				if e != nil {
					received <- e
					return
				}
				defer ws.Close()
				ws.SetReadLimit(MaxControlMessageSize)
				for i := 0; i < 100; i++ {
					_, b, e := ReadWSMessage(ws, MaxControlMessageSize)
					if e != nil {
						received <- e
						return
					}
					if !bytes.Equal(b, bytes.Repeat([]byte("\x1b[38;2;80;90;100m中文 terminal repaint \x1b[0m\r\n"), 90)) {
						received <- errors.New("bytes changed")
						return
					}
				}
				_, _, e = ReadWSMessage(ws, MaxControlMessageSize)
				received <- e
			}))
			defer server.Close()
			var counted *countedConn
			dialer := websocket.Dialer{EnableCompression: compression, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				c, e := (&net.Dialer{}).DialContext(ctx, network, address)
				counted = &countedConn{Conn: c}
				return counted, e
			}}
			ws, response, e := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if e != nil {
				t.Fatal(e)
			}
			defer ws.Close()
			negotiated := strings.Contains(response.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")
			if negotiated != compression {
				t.Fatalf("negotiation %v", negotiated)
			}
			ConfigureCompression(ws)
			startBytes := counted.written.Load()
			start := time.Now()
			payload := bytes.Repeat([]byte("\x1b[38;2;80;90;100m中文 terminal repaint \x1b[0m\r\n"), 90)
			for i := 0; i < 100; i++ {
				if e := ws.WriteMessage(websocket.BinaryMessage, payload); e != nil {
					t.Fatal(e)
				}
			}
			elapsed := time.Since(start)
			wire := counted.written.Load() - startBytes
			if compression && wire >= int64(len(payload)*100)/3 {
				t.Fatalf("compression ineffective: %d", wire)
			}
			t.Logf("100 terminal messages: payload=%d wire=%d elapsed=%s", len(payload)*100, wire, elapsed)
			// A compressed message fits the wire limit but exceeds the decoded limit.
			_ = ws.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("x"), MaxControlMessageSize+1))
			select {
			case e := <-received:
				if !errors.Is(e, websocket.ErrReadLimit) {
					t.Fatalf("expansion accepted: %v", e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("read limit hung")
			}
		})
	}
}

func TestFileBase64CompressionAndSmallControls(t *testing.T) {
	for _, raw := range [][]byte{bytes.Repeat([]byte("workspace source code 中文\n"), 6000), func() []byte {
		b := make([]byte, 192<<10)
		for i := range b {
			b[i] = byte(i * 31)
		}
		return b
	}()} {
		data := []byte(`{"v":1,"type":"file.write","payload":{"data":"` + base64.StdEncoding.EncodeToString(raw) + `"}}`)
		if !CompressWSMessage(false, data) {
			t.Fatal("file bypassed compression")
		}
	}
	if CompressWSMessage(false, []byte(`{"type":"agent.auth","payload":"`+strings.Repeat("x", 2000)+`"}`)) {
		t.Fatal("compressed authentication")
	}
	if CompressWSMessage(true, []byte{1, byte(FrameStdin)}) {
		t.Fatal("compressed keystroke")
	}
}
