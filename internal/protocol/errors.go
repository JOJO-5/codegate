package protocol

import "errors"

// 编解码层的哨兵错误。用 errors.Is 判断，不要比较字符串。
var (
	// ErrVersionMismatch 表示协议版本不兼容。
	ErrVersionMismatch = errors.New("protocol: 版本不兼容")

	// ErrInvalidMessage 表示信封结构非法、类型未知或方向不符。
	ErrInvalidMessage = errors.New("protocol: 消息非法")

	// ErrMessageTooLarge 表示控制消息超出大小上限。
	ErrMessageTooLarge = errors.New("protocol: 消息过大")

	// ---- 二进制帧专用（§16.4）----

	// ErrShortFrame 表示字节数不足以容纳 20 字节固定头。
	ErrShortFrame = errors.New("protocol: 帧长度不足")

	// ErrFrameVersion 表示帧头里的版本号本端不认识。
	ErrFrameVersion = errors.New("protocol: 帧版本不匹配")

	// ErrUnknownFrameType 表示帧类型不在已知集合内。
	ErrUnknownFrameType = errors.New("protocol: 未知帧类型")

	// ErrBadFrameFlags 表示出现了本端不认识的 flag 位。
	// ★ 必须严格拒绝，不能"忽略未知位"：否则未来新增 flag 时会产生语义歧义。
	ErrBadFrameFlags = errors.New("protocol: 帧 flag 非法")

	// ErrFrameDirection 表示帧的流向不对（例如浏览器发了 stdout）。
	ErrFrameDirection = errors.New("protocol: 帧流向非法")
)

// ErrorCode 是协议级错误码，会随 `error` 消息发给对端。
//
// ★ 这些字符串是**协议的一部分**，前端会按它做分支。改名字等于破坏兼容性。
type ErrorCode string

const (
	CodeInvalidMessage  ErrorCode = "invalid_message"
	CodeInvalidPayload  ErrorCode = "invalid_payload"
	CodeUnauthenticated ErrorCode = "unauthenticated"
	CodeForbidden       ErrorCode = "forbidden"
	// CodeNotFound 表示目标不存在，**或存在但不属于你**。
	//
	// ★ 两种情况必须合并成同一个码，且必须映射到 HTTP 404 而不是 403。
	//
	// 403 的语义是「这东西存在，但你不能看」—— 它本身就确认了存在性。
	// 攻击者拿一批 UUID 逐个请求，按 403/404 的差异就能画出一张
	// 「哪些 device_id / session_id 真实存在」的清单，再配合其他漏洞使用。
	//
	// 这个码的存在还有一个工程意义：让「不存在」与「无权」在
	// **所有**路径上收敛到同一个出口。之前 REST 有两套写法
	// （devices.Get 走 404、authorizeDevice 走 403），
	// 于是 /devices/{id} 和 /devices/{id}/sessions 对同一台别人的设备
	// 给出了不同状态码 —— 那本身就是那个预言机。
	CodeNotFound           ErrorCode = "not_found"
	CodeDeviceOffline      ErrorCode = "device_offline"
	CodeDeviceNotPaired    ErrorCode = "device_not_paired"
	CodeSessionNotFound    ErrorCode = "session_not_found"
	CodeSessionLimit       ErrorCode = "session_limit_reached"
	CodeCwdNotAllowed      ErrorCode = "cwd_not_allowed"
	CodeCommandNotAllowed  ErrorCode = "command_not_allowed"
	CodeAlreadyAttached    ErrorCode = "session_already_attached"
	CodeFrameTooLarge      ErrorCode = "frame_too_large"
	CodeRateLimited        ErrorCode = "rate_limited"
	CodeInternal           ErrorCode = "internal"
	CodeVersionUnsupported ErrorCode = "version_unsupported"
)

// CodeError 是**可以安全回传给对端**的错误。
//
// 与哨兵错误的分工：哨兵错误用于本端判断（errors.Is），
// CodeError 用于跨进程通信。永远不要把裸 error 的字符串发给客户端 ——
// 那会把内部实现细节（文件路径、SQL 语句）泄露出去。
type CodeError struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
}

func (e *CodeError) Error() string {
	return string(e.Code) + ": " + e.Message
}

// NewError 构造一个不可重试的错误。
func NewError(code ErrorCode, msg string) *CodeError {
	return &CodeError{Code: code, Message: msg}
}

// NewRetryableError 构造一个可重试的错误（客户端可以退避后重试）。
func NewRetryableError(code ErrorCode, msg string) *CodeError {
	return &CodeError{Code: code, Message: msg, Retryable: true}
}

// Is 让 errors.Is(err, &CodeError{Code: CodeXxx}) 能按错误码匹配。
func (e *CodeError) Is(target error) bool {
	t, ok := target.(*CodeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// ErrorCodeOf 从任意 error 里提取协议错误码，取不到则返回 CodeInternal。
func ErrorCodeOf(err error) ErrorCode {
	var ce *CodeError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return CodeInternal
}
