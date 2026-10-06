package protocol

import (
	"compress/flate"
	"encoding/json"
	"io"

	"github.com/gorilla/websocket"
)

// ReadWSMessage bounds decoded data too: SetReadLimit alone counts compressed
// wire bytes, allowing a tiny deflate message to expand beyond the limit.
func ReadWSMessage(ws *websocket.Conn, limit int64) (int, []byte, error) {
	typ, reader, err := ws.NextReader()
	if err != nil {
		return typ, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err == nil && int64(len(data)) > limit {
		return typ, nil, websocket.ErrReadLimit
	}
	return typ, data, err
}

// Compression is negotiated without context takeover. Tiny keystrokes and
// authentication/control messages bypass compression, keeping interactive
// latency and CPU cost low. Call only from the connection's sole write pump.
func ConfigureCompression(ws *websocket.Conn) { _ = ws.SetCompressionLevel(flate.BestSpeed) }
func CompressWSMessage(binary bool, data []byte) bool {
	if len(data) < 512 {
		return false
	}
	if binary {
		return len(data) >= BinaryHeaderLen && (FrameType(data[1]) == FrameStdout || FrameType(data[1]) == FrameBuffer)
	}
	var env struct {
		Type Type `json:"type"`
	}
	if json.Unmarshal(data, &env) != nil {
		return false
	}
	return env.Type == TypeFileWrite || env.Type == TypeFileReadResult
}
