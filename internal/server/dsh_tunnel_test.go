package server

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestDSHTunnelDrainsLargeResponseBeforeEOF(t *testing.T) {
	s := &Server{web: newWebGateway()}
	ac := newAgentConn(nil, 16)
	ac.DeviceID = strings.Repeat("a", 32)
	conn, err := s.openWebStream(context.Background(), ac)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	open := <-ac.bulk
	env, err := protocol.Decode(open.data)
	if err != nil {
		t.Fatal(err)
	}
	p, err := protocol.DecodePayload[protocol.WebStreamPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	// More than the bounded queue's capacity, with the reader initially idle.
	payload := bytes.Repeat([]byte("asset-data\n"), 100000)
	sent := make(chan error, 1)
	go func() {
		for offset := 0; offset < len(payload); offset += 16 << 10 {
			end := offset + (16 << 10)
			if end > len(payload) {
				end = len(payload)
			}
			frame, e := protocol.EncodeFrame(protocol.FrameWebToServer, 0, uuid.MustParse(p.StreamID), payload[offset:end])
			if e != nil {
				sent <- e
				return
			}
			if e = s.webFrame(ac, frame); e != nil {
				sent <- e
				return
			}
		}
		s.finishWebStream(uuid.MustParse(p.StreamID), ac)
		sent <- nil
	}()
	time.Sleep(20 * time.Millisecond)
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("truncated asset: got %d bytes, want %d", len(got), len(payload))
	}
}
