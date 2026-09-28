package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	req, err := NewRequest("req-1", TypeSessionCreate, "", SessionCreatePayload{
		DeviceID:  "dev-1",
		CommandID: "claude",
		Cwd:       "D:\\Projects\\CodeGate",
		Cols:      120,
		Rows:      30,
		Resume:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeSessionCreate || got.RequestID != "req-1" {
		t.Fatalf("信封字段错误: %+v", got)
	}

	p, err := DecodePayload[SessionCreatePayload](got)
	if err != nil {
		t.Fatal(err)
	}
	if p.CommandID != "claude" || !p.Resume || p.Cols != 120 {
		t.Fatalf("payload 错误: %+v", p)
	}
}

// 响应必须自动继承请求的 SessionID —— 免得调用方漏传导致日志对不上。
func TestNewReplyInheritsSessionID(t *testing.T) {
	req, _ := NewRequest("req-9", TypeSessionResize, "sess-abc", SessionResizePayload{
		SessionID: "sess-abc", Cols: 80, Rows: 24,
	})
	reply, err := NewReply(req, TypeSessionAttached, SessionAttachedPayload{Role: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != "req-9" {
		t.Errorf("ReplyTo = %q, 期望 req-9", reply.ReplyTo)
	}
	if reply.SessionID != "sess-abc" {
		t.Errorf("SessionID 未继承: %q", reply.SessionID)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid := `{"v":1,"type":"ping","payload":{}}`

	unknownType := `{"v":1,"type":"agent.evil"}`
	badVersion := `{"v":99,"type":"ping"}`
	badJSON := `{"v":1,"type":`
	badPayload := `{"v":1,"type":"ping","payload":"不是对象但仍是合法JSON"}`

	tests := []struct {
		name string
		in   string
		want error
	}{
		{"空", "", ErrInvalidMessage},
		{"坏 JSON", badJSON, ErrInvalidMessage},
		{"未知类型", unknownType, ErrInvalidMessage},
		{"版本过高", badVersion, ErrVersionMismatch},
		{"合法", valid, nil},
		{"payload 是字符串（合法 JSON，允许）", badPayload, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
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

func TestDecodeRejectsOversize(t *testing.T) {
	big := make([]byte, MaxControlMessageSize+1)
	_, err := Decode(big)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("超限应报 ErrMessageTooLarge，实际 %v", err)
	}
}

// 未知字段必须被容忍，否则协议没法演进（§9.4）。
func TestDecodeToleratesUnknownFields(t *testing.T) {
	in := `{"v":1,"type":"ping","payload":{},"future_field":"x","nested":{"a":1}}`
	if _, err := Decode([]byte(in)); err != nil {
		t.Fatalf("未知字段不该导致失败: %v", err)
	}
}

// ★ 方向校验是安全闸门：浏览器不能发 Agent 的事件。
func TestValidateFrom(t *testing.T) {
	tests := []struct {
		typ  Type
		from Sender
		ok   bool
	}{
		{TypeSessionCreate, SentByClient, true},
		{TypeSessionCreate, SentByAgent, false},
		{TypeSessionExit, SentByAgent, true},
		{TypeSessionExit, SentByClient, false}, // ★ 客户端伪造会话退出
		{TypeAgentReady, SentByServer, true},
		{TypeAgentReady, SentByAgent, false}, // Agent 不能自己宣布就绪
		{TypeAgentReady, SentByClient, false},
		{TypeSessionAttach, SentByClient, true},
		{TypeSessionAttached, SentByAgent, true},
		{TypeFileRead, SentByClient, true},
		{TypeFileRead, SentByAgent, false},
		{TypeFileListed, SentByAgent, true},
		{TypeFileListed, SentByClient, false},
		{TypeFileAck, SentByAgent, true},
		{TypeFileAck, SentByClient, true},
		{TypeError, SentByServer, true},
		{Type("nope"), SentByAgent, false},
	}
	for _, tt := range tests {
		err := ValidateFrom(tt.typ, tt.from)
		if tt.ok && err != nil {
			t.Errorf("ValidateFrom(%s, %s) 期望通过，实际 %v", tt.typ, tt.from, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateFrom(%s, %s) 期望报错，实际通过", tt.typ, tt.from)
		}
	}
}

func TestValidateFromSide(t *testing.T) {
	// agent 连接上收到浏览器才能发的消息 → 拒绝
	if err := ValidateFromSide(TypeSessionCreate, SideAgentConn); err == nil {
		t.Error("agent 连接不该能发 session.create")
	}
	if err := ValidateFromSide(TypeSessionCreate, SideClientConn); err != nil {
		t.Errorf("client 连接应该能发 session.create，实际 %v", err)
	}
	// 反过来
	if err := ValidateFromSide(TypeSessionExit, SideAgentConn); err != nil {
		t.Errorf("agent 连接应该能发 session.exit，实际 %v", err)
	}
	if err := ValidateFromSide(TypeSessionExit, SideClientConn); err == nil {
		t.Error("★ client 连接不该能发 session.exit")
	}
}

// 错误消息绝不能把内部细节透给对端。
func TestNewErrorEnvelopeHidesInternals(t *testing.T) {
	internal := errors.New("open C:\\Users\\JOJO\\secret.key: access denied")
	env := NewErrorEnvelope("req-1", internal)

	p, err := DecodePayload[ErrorPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != CodeInternal {
		t.Errorf("code = %v, 期望 %v", p.Code, CodeInternal)
	}
	if strings.Contains(p.Message, "secret.key") {
		t.Fatalf("★ 内部路径泄露到对端: %q", p.Message)
	}
	if env.ReplyTo != "req-1" {
		t.Errorf("ReplyTo = %q", env.ReplyTo)
	}
}

func TestNewErrorEnvelopePreservesCodeError(t *testing.T) {
	env := NewErrorEnvelope("", NewError(CodeSessionNotFound, "会话不存在"))
	p, _ := DecodePayload[ErrorPayload](env)
	if p.Code != CodeSessionNotFound || p.Message != "会话不存在" {
		t.Fatalf("CodeError 未原样传递: %+v", p)
	}
}

func TestCodeErrorIs(t *testing.T) {
	err := NewRetryableError(CodeRateLimited, "太快了")
	if !errors.Is(err, &CodeError{Code: CodeRateLimited}) {
		t.Error("errors.Is 应能按错误码匹配")
	}
	if errors.Is(err, &CodeError{Code: CodeForbidden}) {
		t.Error("错误码不该误匹配")
	}
	if got := ErrorCodeOf(err); got != CodeRateLimited {
		t.Errorf("ErrorCodeOf = %v", got)
	}
}

func TestVersionNegotiate(t *testing.T) {
	if v, err := Negotiate(Current()); err != nil || v != MaxSupported {
		t.Fatalf("同版本协商失败: v=%d err=%v", v, err)
	}
	// 对端只支持未来版本 → 不兼容
	if _, err := Negotiate(VersionInfo{Min: 99, Max: 100}); !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("未来版本应报 ErrVersionMismatch，实际 %v", err)
	}
	// 对端跨版本区间，包含本端 → 应取交集上限
	if v, err := Negotiate(VersionInfo{Min: 1, Max: 5}); err != nil || v != MaxSupported {
		t.Errorf("区间交集协商错误: v=%d err=%v", v, err)
	}
}

func FuzzDecodeEnvelope(f *testing.F) {
	f.Add([]byte(`{"v":1,"type":"ping"}`))
	f.Add([]byte(`{"v":1,"type":"session.create","request_id":"a","payload":{"cwd":"/"}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, b []byte) {
		env, err := Decode(b)
		if err != nil {
			return
		}
		// 解析成功 → 必须能重新编码
		if _, err := Encode(env); err != nil {
			t.Fatalf("解析成功但编码失败: %v", err)
		}
		// payload 必须是合法 JSON
		if len(env.Payload) > 0 && !json.Valid(env.Payload) {
			t.Fatalf("payload 不是合法 JSON: %s", env.Payload)
		}
	})
}
