package protocol

import (
	"encoding/json"
	"fmt"
)

// Encode 把信封序列化成可发送的字节。
//
// 编码后仍会检查一次大小：payload 是外部传入的，可能在 Marshal 之后才变大。
func Encode(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("%w: 信封为空", ErrInvalidMessage)
	}
	if !Valid(env.Type) {
		return nil, fmt.Errorf("%w: 未知消息类型 %q", ErrInvalidMessage, env.Type)
	}
	if env.V == 0 {
		env.V = MaxSupported
	}
	if env.V < MinSupported || env.V > MaxSupported {
		return nil, fmt.Errorf("%w: 版本 %d 不在支持范围 %d-%d",
			ErrVersionMismatch, env.V, MinSupported, MaxSupported)
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("protocol: 序列化消息失败: %w", err)
	}
	if len(data) > MaxControlMessageSize {
		return nil, fmt.Errorf("%w: %d 字节超过上限 %d",
			ErrMessageTooLarge, len(data), MaxControlMessageSize)
	}
	return data, nil
}

// Decode 解析一条控制消息并做完整校验。
//
// 校验顺序刻意从廉价到昂贵：先长度、再 JSON、再语义。
// 这样畸形报文在第一步就被挡住，不会走到解析器内部。
func Decode(data []byte) (*Envelope, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: 空消息", ErrInvalidMessage)
	}
	if len(data) > MaxControlMessageSize {
		return nil, fmt.Errorf("%w: %d 字节超过上限 %d",
			ErrMessageTooLarge, len(data), MaxControlMessageSize)
	}

	var env Envelope
	// 用 json.Unmarshal 而不是 Decoder：不需要流式，且能直接拿到错误。
	// 不开 DisallowUnknownFields —— 未知字段要容忍，否则协议没法演进。
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: JSON 解析失败: %v", ErrInvalidMessage, err)
	}

	if env.V < MinSupported || env.V > MaxSupported {
		return nil, fmt.Errorf("%w: 收到版本 %d，本端支持 %d-%d",
			ErrVersionMismatch, env.V, MinSupported, MaxSupported)
	}
	if !Valid(env.Type) {
		return nil, fmt.Errorf("%w: 未知消息类型 %q", ErrInvalidMessage, env.Type)
	}
	if len(env.Payload) > 0 && !json.Valid(env.Payload) {
		return nil, fmt.Errorf("%w: payload 不是合法 JSON", ErrInvalidPayload)
	}
	return &env, nil
}

// DecodeFrom 解析并按连接侧校验发出方。
//
// ★ 这是 Agent/Client 两个 WS handler 唯一该用的入口。
// 用裸 Decode 会漏掉方向校验，等于给伪造事件留后门。
func DecodeFrom(data []byte, side Side) (*Envelope, error) {
	env, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if err := ValidateFromSide(env.Type, side); err != nil {
		return nil, err
	}
	return env, nil
}

// NewErrorEnvelope 构造一条 error 消息。
//
// 传入的 error 若不是 *CodeError，一律折叠成 CodeInternal，
// **绝不把原始错误字符串透给对端** —— 那会泄露文件路径、SQL 等实现细节。
func NewErrorEnvelope(replyTo string, err error) *Envelope {
	var ce *CodeError
	if e, ok := err.(*CodeError); ok {
		ce = e
	} else if extracted := ErrorCodeOf(err); extracted != CodeInternal {
		ce = &CodeError{Code: extracted, Message: err.Error()}
	} else {
		ce = &CodeError{Code: CodeInternal, Message: "internal error"}
	}

	env, _ := NewEnvelope(TypeError, ErrorPayload{
		Code:      ce.Code,
		Message:   ce.Message,
		Retryable: ce.Retryable,
	})
	env.ReplyTo = replyTo
	return env
}
