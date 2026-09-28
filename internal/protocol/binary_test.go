package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAppendFrameRoundTrip(t *testing.T) {
	sid := uuid.MustParse("018f3e2a-1111-7abc-8def-0123456789ab")
	payload := []byte("hello \x1b[31mworld\x1b[0m 中文 🎉 \x00\xff")

	frame, err := AppendFrame(nil, FrameStdout, FlagDropped, sid, payload)
	if err != nil {
		t.Fatalf("AppendFrame: %v", err)
	}
	if len(frame) != BinaryHeaderLen+len(payload) {
		t.Fatalf("帧长 = %d, 期望 %d", len(frame), BinaryHeaderLen+len(payload))
	}

	got, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if got.Type != FrameStdout {
		t.Errorf("Type = %v, 期望 %v", got.Type, FrameStdout)
	}
	if got.Flags != FlagDropped {
		t.Errorf("Flags = %#x, 期望 %#x", got.Flags, FlagDropped)
	}
	if got.StreamID != sid {
		t.Errorf("StreamID = %v, 期望 %v", got.StreamID, sid)
	}
	// ★ 字节必须完全一致：终端流允许任意二进制，包括非法 UTF-8
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("Payload 不一致:\n got %q\nwant %q", got.Payload, payload)
	}
}

// 非 UTF-8 字节必须原样透传 —— 这是 §16.3 的核心要求。
func TestFrameIsBinaryTransparent(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 0x7f, 0x1b, 0x5b}
	frame, err := AppendFrame(nil, FrameStdout, 0, uuid.New(), raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Payload, raw) {
		t.Fatalf("二进制内容被破坏: %v", got.Payload)
	}
}

// 复用 dst 是高频路径的关键优化，必须验证它真的复用且不污染旧内容。
func TestAppendFrameReusesBuffer(t *testing.T) {
	buf := make([]byte, 0, 1024)
	base := &buf[:1][0] // 拿底层数组地址做参考

	f1, err := AppendFrame(buf, FrameStdin, 0, uuid.New(), []byte("aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	addr1 := &f1[0]
	if addr1 != base {
		t.Error("AppendFrame 没有复用传入的 dst 底层数组")
	}

	// 清空长度再追加，应该还是在同一块内存上
	f2, err := AppendFrame(f1[:0], FrameStdin, 0, uuid.New(), []byte("bbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if &f2[0] != base {
		t.Error("第二次 AppendFrame 换了新数组，复用失效")
	}
	got, err := DecodeFrame(f2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != "bbbb" {
		t.Errorf("复用后内容错误: %q", got.Payload)
	}
}

func TestDecodeFrameErrors(t *testing.T) {
	valid, _ := AppendFrame(nil, FrameStdout, 0, uuid.New(), []byte("x"))

	short := valid[:BinaryHeaderLen-1]

	badVersion := bytes.Clone(valid)
	badVersion[0] = 0x09

	badType := bytes.Clone(valid)
	badType[1] = 0x7f

	badFlags := bytes.Clone(valid)
	binary.BigEndian.PutUint16(badFlags[2:4], 0x8000) // 未知 flag 位

	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"空切片", nil, ErrShortFrame},
		{"长度不足", short, ErrShortFrame},
		{"恰好只有头", valid[:BinaryHeaderLen], nil}, // 空 payload 是合法的
		{"版本不匹配", badVersion, ErrFrameVersion},
		{"未知类型", badType, ErrUnknownFrameType},
		{"未知 flag 位", badFlags, ErrBadFrameFlags},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeFrame(tt.in)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("期望成功，实际 %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, 期望 %v", err, tt.want)
			}
		})
	}
}

func TestAppendFrameRejectsBadInput(t *testing.T) {
	if _, err := AppendFrame(nil, 0x99, 0, uuid.New(), nil); !errors.Is(err, ErrUnknownFrameType) {
		t.Errorf("未知类型应报错，实际 %v", err)
	}
	if _, err := AppendFrame(nil, FrameStdin, 0x4000, uuid.New(), nil); !errors.Is(err, ErrBadFrameFlags) {
		t.Errorf("未知 flag 应报错，实际 %v", err)
	}
}

// 方向校验是防伪造的第一道闸门。
func TestFrameDirection(t *testing.T) {
	tests := []struct {
		typ  FrameType
		from Sender
		ok   bool
	}{
		{FrameStdin, SentByClient, true},
		{FrameStdin, SentByAgent, false}, // Agent 不能伪造用户输入
		{FrameStdout, SentByAgent, true},
		{FrameStdout, SentByClient, false}, // ★ 浏览器不能伪造 stdout 注入
		{FrameBuffer, SentByAgent, true},
		{FrameBuffer, SentByClient, false},
		{FrameFileData, SentByAgent, true},  // 下载
		{FrameFileData, SentByClient, true}, // 上传
		{FrameFileData, SentByServer, false},
		{0x77, SentByAgent, false},
	}
	for _, tt := range tests {
		err := ValidateFrameFrom(tt.typ, tt.from)
		if tt.ok && err != nil {
			t.Errorf("ValidateFrameFrom(%v, %s) 期望通过，实际 %v", tt.typ, tt.from, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateFrameFrom(%v, %s) 期望报错，实际通过", tt.typ, tt.from)
		}
	}
}

func TestPeekFrameHeader(t *testing.T) {
	sid := uuid.New()
	frame, _ := AppendFrame(nil, FrameBuffer, FlagBufferEnd|FlagFinal, sid, []byte("payload"))

	typ, flags, got, err := PeekFrameHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameBuffer || got != sid {
		t.Errorf("头部解析错误: typ=%v sid=%v", typ, got)
	}
	if flags&FlagBufferEnd == 0 || flags&FlagFinal == 0 {
		t.Errorf("flags = %#x, 期望含 BUFFER_END|FINAL", flags)
	}
	// Peek 不应该碰 payload：截断到只有头也应该成功
	if _, _, _, err := PeekFrameHeader(frame[:BinaryHeaderLen]); err != nil {
		t.Errorf("只有头时应成功，实际 %v", err)
	}
}

// 模糊测试：畸形输入不得 panic、不得 OOM。
func FuzzDecodeFrame(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0x01},
		{0x01, 0x02, 0x00, 0x00},
		make([]byte, BinaryHeaderLen),
		bytes.Repeat([]byte{0xff}, BinaryHeaderLen+8),
	}
	if valid, err := AppendFrame(nil, FrameStdout, FlagBufferEnd, uuid.New(), []byte("seed")); err == nil {
		seeds = append(seeds, valid)
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := DecodeFrame(b)
		if err != nil {
			return
		}
		// 解析成功时必须满足：payload 正好是头之后的部分
		if len(b) != BinaryHeaderLen+len(fr.Payload) {
			t.Fatalf("payload 长度不对: %d + %d != %d",
				BinaryHeaderLen, len(fr.Payload), len(b))
		}
		// 重新编码必须逐字节一致
		out, err := AppendFrame(nil, fr.Type, fr.Flags, fr.StreamID, fr.Payload)
		if err != nil {
			t.Fatalf("重新编码失败: %v", err)
		}
		if !bytes.Equal(out, b) {
			t.Fatalf("round-trip 不一致\n in: %x\nout: %x", b, out)
		}
	})
}
