package server

import (
	"context"
	"errors"
	"net"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

// A net.Pipe endpoint gives ReverseProxy a real net.Conn, including Upgrade
// hijacking for DSH's WebSockets. The other endpoint is multiplexed through
// the existing authenticated Agent WebSocket in bounded 16 KiB chunks.
func (s *Server) openWebStream(ctx context.Context, ac *AgentConn) (net.Conn, error) {
	client, relay := net.Pipe()
	id := uuid.New()
	stream := &proxyStream{
		agent: ac, client: client, pipe: relay,
		queue: make(chan []byte, 16), done: make(chan struct{}),
	}
	s.web.mu.Lock()
	count := 0
	for _, other := range s.web.streams {
		if other.agent == ac { count++ }
	}
	if count >= 32 {
		s.web.mu.Unlock()
		_ = client.Close(); _ = relay.Close()
		return nil, errors.New("too many DSH Web connections")
	}
	s.web.streams[id] = stream
	s.web.mu.Unlock()
	env, err := protocol.NewEnvelope(protocol.TypeWebOpen, protocol.WebStreamPayload{StreamID:id.String()})
	if err == nil {
		var raw []byte
		raw, err = protocol.Encode(env)
		if err == nil { err = ac.TrySendText(raw) }
	}
	if err != nil {
		s.closeWebStream(id, false)
		return nil, err
	}
	go func() {
		buf := make([]byte, 16<<10)
		defer s.closeWebStream(id, true)
		for {
			n, err := relay.Read(buf)
			if n > 0 {
				frame, frameErr := protocol.EncodeFrame(protocol.FrameWebToAgent, 0, id, buf[:n])
				if frameErr != nil || ac.SendWeb(frame) != nil { return }
			}
			if err != nil { return }
		}
	}()
	go func() {
		for {
			select {
			case <-stream.done: return
			case data := <-stream.queue:
				if len(data) == 0 { continue }
				if _, err := relay.Write(data); err != nil { s.closeWebStream(id, true); return }
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done(): s.closeWebStream(id, true)
		case <-ac.Done(): s.closeWebStream(id, false)
		case <-stream.done:
		}
	}()
	return client, nil
}

func (s *Server) closeWebStream(id uuid.UUID, notify bool) {
	s.web.mu.Lock()
	stream := s.web.streams[id]
	delete(s.web.streams, id)
	s.web.mu.Unlock()
	if stream == nil { return }
	stream.once.Do(func() {
		close(stream.done)
		_ = stream.client.Close()
		_ = stream.pipe.Close()
	})
	if notify {
		env, err := protocol.NewEnvelope(protocol.TypeWebClose, protocol.WebStreamPayload{StreamID:id.String()})
		if err == nil {
			if raw, err := protocol.Encode(env); err == nil { _ = stream.agent.TrySendText(raw) }
		}
	}
}

func (s *Server) webFrame(ac *AgentConn, data []byte) error {
	f, err := protocol.DecodeFrame(data)
	if err != nil || f.Type != protocol.FrameWebToServer || len(f.Payload) > 16<<10 {
		return errors.New("invalid DSH Web frame")
	}
	s.web.mu.Lock()
	stream := s.web.streams[f.StreamID]
	s.web.mu.Unlock()
	if stream == nil { return nil } // stream was cancelled; late frame
	if stream.agent != ac { return errors.New("DSH Web frame belongs to another Agent") }
	buf := append([]byte(nil), f.Payload...)
	select {
	case <-stream.done:
	case stream.queue <- buf:
	default:
		s.closeWebStream(f.StreamID, true)
	}
	return nil
}

func (s *Server) closeWebAgent(ac *AgentConn) {
	s.web.mu.Lock()
	if current := s.web.upstreams[ac.DeviceID]; current.agent == ac {
		delete(s.web.upstreams, ac.DeviceID)
	}
	var ids []uuid.UUID
	for id, stream := range s.web.streams {
		if stream.agent == ac { ids = append(ids, id) }
	}
	s.web.mu.Unlock()
	for _, id := range ids { s.closeWebStream(id, false) }
}
