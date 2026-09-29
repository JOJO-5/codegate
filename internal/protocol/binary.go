package protocol

import (
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"
)

// 二进制帧（§16）。终端数据和文件数据共用同一套帧格式。
//
//	┌─────────┬────────┬────────┬──────────────────┬───────────────┐
//	│ Version │  Type  │ Flags  │     StreamID     │    Payload    │
//	│  1 byte │ 1 byte │ 2 byte │     16 bytes     │    N bytes    │
//	│  offset0│    1   │   2-3  │       4-19       │      20..     │
//	└─────────┴────────┴────────┴──────────────────┴───────────────┘
//
// 头长固定 20 字节。
//
// # 为什么字段叫 StreamID 而不是 SessionID
//
// 那 16 字节本来就是"这一帧属于哪个流"。终端和文件都是流，只是流的标识不同：
//
//	终端帧：StreamID = SessionID
//	文件帧：StreamID = TransferID（见 docs/PHASE0-FILE-TRANSPORT.md §3.1）
//
// 换个名字就把两种用途统一了，字节布局零改动。**这不是 hack，是把字段的本意说清楚。**
//
// 为什么头里【没有】PayloadLength：WebSocket 帧本身就携带长度，
// 再加一个只会引入"两个长度不一致怎么办"这个多余的解析攻击面。
// 将来真要做多路复用（一帧塞多个逻辑帧）时再加，并且必须同时递增 Version。
const (
	// BinaryHeaderLen 是固定头长度。
	BinaryHeaderLen = 20

	binaryVersion1 = 0x01
)

// FrameType 是二进制帧的类型。
type FrameType uint8

const (
	// FrameStdin 是浏览器 → PTY 的输入字节。
	FrameStdin FrameType = 0x01
	// FrameStdout 是 PTY → 浏览器的实时输出。
	FrameStdout FrameType = 0x02
	// FrameBuffer 是 PTY → 浏览器的历史重放（attach 时）。
	FrameBuffer FrameType = 0x03

	// FrameFileData 是文件传输的字节块。**双向**。
	//
	// 下载时 Agent → 浏览器；上传时浏览器 → Agent。
	// 因为方向取决于这次传输的类型，静态方向表表达不了 ——
	// 必须由 Server 侧的 transfer 注册表做【每传输方向锁】
	// （见 docs/PHASE0-FILE-TRANSPORT.md §3.3）。
	FrameFileData FrameType = 0x10
	FrameWebToAgent FrameType = 0x20
	FrameWebToServer FrameType = 0x21
)

// 帧标志位。
const (
	// FlagBufferEnd 表示重放结束，后续就是实时流了。
	FlagBufferEnd uint16 = 1 << 0
	// FlagDropped 表示该会话有字节被丢弃，客户端应做一次全量重放。
	FlagDropped uint16 = 1 << 1
	// FlagFinal 表示本帧是该传输的最后一块（文件传输用）。
	FlagFinal uint16 = 1 << 2

	// knownFlags 是全部已知标志位。校验时用它做掩码，
	// ★ 未知位必须拒绝而不是忽略 —— 否则未来新增 flag 会产生语义歧义。
	knownFlags = FlagBufferEnd | FlagDropped | FlagFinal
)

func (t FrameType) String() string {
	switch t {
	case FrameStdin:
		return "stdin"
	case FrameStdout:
		return "stdout"
	case FrameBuffer:
		return "buffer"
	case FrameFileData:
		return "file.data"
	case FrameWebToAgent:
		return "web.to_agent"
	case FrameWebToServer:
		return "web.to_server"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(t))
	}
}

// knownFrameType 判断帧类型是否已知。
func knownFrameType(t FrameType) bool {
	switch t {
	case FrameStdin, FrameStdout, FrameBuffer, FrameFileData, FrameWebToAgent, FrameWebToServer:
		return true
	default:
		return false
	}
}

// CanSendFrame 判断某个身份是否有权发出该类型的帧。
//
// ★ 对终端帧，方向校验是防"客户端伪造 stdout 注入"的关键：
// 浏览器只能发 stdin，Agent 只能发 stdout/buffer。
//
// ★ 对文件帧，这里只能返回"双方都可能" —— 真正的方向判定必须结合
// transfer 的类型（下载还是上传）。调用方（Server）拿到 true 之后
// **仍然必须**做每传输方向锁。这个函数只负责挡住明显非法的组合。
func CanSendFrame(t FrameType, from Sender) bool {
	switch t {
	case FrameStdin:
		return from == SentByClient
	case FrameStdout, FrameBuffer:
		return from == SentByAgent
	case FrameFileData:
		// 双向。真正的判定在 Server 的 transfer 注册表里。
		return from == SentByAgent || from == SentByClient
	case FrameWebToAgent:
		return from == SentByServer
	case FrameWebToServer:
		return from == SentByAgent
	default:
		return false
	}
}

// ValidateFrameFrom 校验帧的发出方是否合法。
func ValidateFrameFrom(t FrameType, from Sender) error {
	if !knownFrameType(t) {
		return fmt.Errorf("%w: 0x%02x", ErrUnknownFrameType, uint8(t))
	}
	if !CanSendFrame(t, from) {
		return fmt.Errorf("%w: %s 帧不允许由 %s 发出", ErrFrameDirection, t, from)
	}
	return nil
}

// FrameSize 返回编码后的总长度，便于调用方预分配缓冲。
func FrameSize(payloadLen int) int { return BinaryHeaderLen + payloadLen }

// AppendFrame 把一帧追加到 dst 上并返回。
//
// 复用 dst 的底层数组是本函数存在的理由：终端输出是高频路径，
// 每帧一次 append 而不是每次重新分配，能显著压低 GC 压力（§25）。
// dst 传 nil 时会新分配。
//
// 编码结果永远是 dst 的返回值，调用方必须使用返回值。
func AppendFrame(dst []byte, typ FrameType, flags uint16, sid uuid.UUID, payload []byte) ([]byte, error) {
	if !knownFrameType(typ) {
		return dst, fmt.Errorf("%w: 0x%02x", ErrUnknownFrameType, uint8(typ))
	}
	if flags&^knownFlags != 0 {
		return dst, fmt.Errorf("%w: flags=0x%04x", ErrBadFrameFlags, flags)
	}

	base := len(dst)
	dst = append(dst, make([]byte, BinaryHeaderLen+len(payload))...)
	hdr := dst[base:]

	hdr[0] = binaryVersion1
	hdr[1] = byte(typ)
	binary.BigEndian.PutUint16(hdr[2:4], flags)
	copy(hdr[4:20], sid[:])
	copy(hdr[BinaryHeaderLen:], payload)

	return dst, nil
}

// Frame 是解析后的二进制帧。
type Frame struct {
	Type  FrameType
	Flags uint16
	// StreamID 的含义按 Type 解释：终端帧里是 SessionID，文件帧里是 TransferID。
	StreamID uuid.UUID
	// Payload 是 src 的子切片，**没有复制**。
	// 调用方在使用它期间不得复用或修改 src。
	Payload []byte
}

// SessionID 把 StreamID 当会话标识读。仅对终端帧有意义。
func (f Frame) SessionID() uuid.UUID { return f.StreamID }

// TransferID 把 StreamID 当文件传输标识读。仅对文件帧有意义。
func (f Frame) TransferID() uuid.UUID { return f.StreamID }

// HasFlag 判断某个标志位是否置位。
func (f Frame) HasFlag(flag uint16) bool { return f.Flags&flag != 0 }

// DecodeFrame 解析一帧并做完整校验。
//
// 校验顺序从廉价到昂贵，且**不做任何容错**：任何一条不过就直接返回错误，
// 由上层断开连接。这里宽容一次，后面就要到处打补丁。
func DecodeFrame(src []byte) (Frame, error) {
	var f Frame

	if len(src) < BinaryHeaderLen {
		return f, fmt.Errorf("%w: 只有 %d 字节，需要至少 %d",
			ErrShortFrame, len(src), BinaryHeaderLen)
	}
	if src[0] != binaryVersion1 {
		return f, fmt.Errorf("%w: 帧版本 0x%02x", ErrFrameVersion, src[0])
	}

	typ := FrameType(src[1])
	if !knownFrameType(typ) {
		return f, fmt.Errorf("%w: 0x%02x", ErrUnknownFrameType, uint8(typ))
	}

	flags := binary.BigEndian.Uint16(src[2:4])
	if flags&^knownFlags != 0 {
		return f, fmt.Errorf("%w: flags=0x%04x", ErrBadFrameFlags, flags)
	}

	f.Type = typ
	f.Flags = flags
	copy(f.StreamID[:], src[4:20])
	f.Payload = src[BinaryHeaderLen:]
	return f, nil
}

// PeekFrameHeader 只解析 20 字节固定头，不碰 payload。
//
// 这是给 Relay 层用的：Server 转发时只需要 StreamID 做路由，
// 不应该、也不需要看到终端内容或文件内容（§4.2、文件传输 §6）。
func PeekFrameHeader(src []byte) (typ FrameType, flags uint16, streamID uuid.UUID, err error) {
	f, err := DecodeFrame(src)
	if err != nil {
		return 0, 0, uuid.Nil, err
	}
	return f.Type, f.Flags, f.StreamID, nil
}

// EncodeFrame 是 AppendFrame 的便捷封装，用于一次性场景。
// 高频路径请直接用 AppendFrame 复用缓冲。
func EncodeFrame(typ FrameType, flags uint16, sid uuid.UUID, payload []byte) ([]byte, error) {
	return AppendFrame(nil, typ, flags, sid, payload)
}
