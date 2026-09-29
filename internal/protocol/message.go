package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MaxControlMessageSize 是单条控制消息（JSON）的大小上限。
//
// 控制消息都是低频小包；1 MB 已经远超任何合法用例。
// 这个限制是防资源耗尽的第一道闸门（§11.2 DoS）。
const MaxControlMessageSize = 1 << 20

// Envelope 是所有控制消息的统一信封（§9.2）。
//
// 字段用 omitempty 而不是指针：这些消息高频且量小，
// 少一次指针解引用、少一次 nil 判断，代码更好读。
type Envelope struct {
	// V 是协议版本，必填。
	V uint8 `json:"v"`
	// Type 是消息类型，必填。
	Type Type `json:"type"`
	// RequestID 由请求方生成，响应方在 ReplyTo 里回填。
	// 推荐用 UUIDv7（时间有序，便于日志排序）。
	RequestID string `json:"request_id,omitempty"`
	// ReplyTo 回填被响应请求的 RequestID。
	ReplyTo string `json:"reply_to,omitempty"`
	// SessionID 在会话相关消息里必填，便于日志与路由。
	SessionID string `json:"session_id,omitempty"`
	// TS 是毫秒时间戳。**仅作参考，不得用于任何逻辑判断** ——
	// 两端的时钟不保证同步。
	TS int64 `json:"ts,omitempty"`
	// Payload 是类型相关的负载。用 RawMessage 是为了延迟解析：
	// 路由层不需要理解 payload 的内容。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WebStartPayload carries the trusted public authority for one DSH Web instance.
// Agent always binds DSH to loopback; this field is only a DSH Host/Origin allowlist entry.
type WebStartPayload struct {
	Host string `json:"host"`
}

// WebStartedPayload holds DSH's browser cookie. It stays on the authenticated
// Server-Agent connection and must never be returned to a browser or logged.
type WebStartedPayload struct {
	Cookie string `json:"cookie"`
}

type WebStreamPayload struct {
	StreamID string `json:"stream_id"`
}

// NewEnvelope 构造一个信封并把 payload 序列化进去。
// payload 传 nil 时 Payload 为空。
func NewEnvelope(t Type, payload any) (*Envelope, error) {
	env := &Envelope{V: MaxSupported, Type: t}
	if payload == nil {
		return env, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("protocol: 序列化 payload 失败: %w", err)
	}
	env.Payload = raw
	return env, nil
}

// NewRequest 构造请求：带 RequestID。
func NewRequest(id string, t Type, sessionID string, payload any) (*Envelope, error) {
	env, err := NewEnvelope(t, payload)
	if err != nil {
		return nil, err
	}
	env.RequestID = id
	env.SessionID = sessionID
	return env, nil
}

// NewReply 构造响应：带 ReplyTo，并继承请求的 SessionID。
//
// 继承 SessionID 是刻意的：响应天然属于同一个会话，
// 让调用方不必每次手工传，也避免漏传导致日志里对不上。
func NewReply(req *Envelope, t Type, payload any) (*Envelope, error) {
	env, err := NewEnvelope(t, payload)
	if err != nil {
		return nil, err
	}
	if req != nil {
		env.ReplyTo = req.RequestID
		env.SessionID = req.SessionID
	}
	return env, nil
}

// DecodePayload 把 Payload 反序列化成 T。
//
// 注意：Go 的 encoding/json 默认忽略未知字段，这对协议演进是**好事**
// （旧代码收到新字段不会报错），所以这里不开 DisallowUnknownFields。
func DecodePayload[T any](env *Envelope) (T, error) {
	var out T
	if env == nil {
		return out, fmt.Errorf("%w: 信封为空", ErrInvalidMessage)
	}
	if len(env.Payload) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(env.Payload, &out); err != nil {
		return out, fmt.Errorf("%w: 解析 %s 的 payload 失败: %v", ErrInvalidPayload, env.Type, err)
	}
	return out, nil
}

// ErrInvalidPayload 表示 payload 结构不对。
var ErrInvalidPayload = errors.New("protocol: payload 非法")

// ---------------------------------------------------------------------------
// 各消息的 payload 结构体
//
// 约定：字段名一律 snake_case；可选字段用 omitempty；
// 时间戳一律 Unix 毫秒（int64），不用 time.Time（跨语言/跨库更省心）。
// ---------------------------------------------------------------------------

// AgentHelloPayload 是 Agent 连上来的第一句话。
type AgentHelloPayload struct {
	Protocol     VersionInfo `json:"protocol"`
	DeviceID     string      `json:"device_id"`
	Name         string      `json:"name"`
	Platform     string      `json:"platform"` // windows | linux | darwin
	Arch         string      `json:"arch"`     // amd64 | arm64
	AgentVersion string      `json:"agent_version"`
	Caps         AgentCaps   `json:"caps"`
}

// AgentCaps 是 Agent 自报的能力上限。
type AgentCaps struct {
	MaxSessions int  `json:"max_sessions"`
	ConPTY      bool `json:"conpty,omitempty"`
	UnixPTY     bool `json:"unix_pty,omitempty"`
}

// AgentChallengePayload 是 Server 发来的随机数，用于防重放。
type AgentChallengePayload struct {
	Nonce      string `json:"nonce"` // base64(32 字节)
	ServerTime int64  `json:"server_time"`
}

// AgentAuthPayload 是 Agent 对 nonce 的 Ed25519 签名。
//
// 签名对象是 `nonce || device_id || server_time` 的字节拼接（§8.1）。
// 带上 device_id 和 server_time 是为了把签名绑死在这台设备和这个时刻上。
type AgentAuthPayload struct {
	Signature string `json:"signature"` // base64(64 字节)
}

// AgentReadyPayload 是 Server 下发的运行参数。
type AgentReadyPayload struct {
	Protocol          VersionInfo `json:"protocol"`
	ServerTime        int64       `json:"server_time"`
	HeartbeatInterval int         `json:"heartbeat_interval"` // 秒
	Limits            AgentLimits `json:"limits"`
}

// AgentLimits 是 Server 强加给 Agent 的资源上限（§35）。
type AgentLimits struct {
	MaxSessions    int `json:"max_sessions"`
	MaxFrameSize   int `json:"max_frame_size"`
	MaxBufferSize  int `json:"max_buffer_size"`
	MaxMessageSize int `json:"max_message_size"`
}

// AgentPairBeginPayload 是 Agent 请求生成配对码。
type AgentPairBeginPayload struct {
	DeviceID     string `json:"device_id"`
	PublicKey    string `json:"public_key"` // base64(32 字节 Ed25519 公钥)
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

// AgentPairCodePayload 是 Server 返回的配对码。
type AgentPairCodePayload struct {
	Code      string `json:"code"` // 形如 7F2K-93LM
	ExpiresIn int    `json:"expires_in"`
}

// AgentPairCompletedPayload 通知 Agent 配对成功。
type AgentPairCompletedPayload struct {
	DeviceID  string `json:"device_id"`
	UserEmail string `json:"user_email,omitempty"`
}

// SessionSummary 是会话的对外快照。
//
// 这个结构同时出现在 Agent 上报、Server 转发、DB 落库三处，
// 所以字段顺序和命名要保持稳定。
type SessionSummary struct {
	SessionID      string   `json:"session_id"`
	DeviceID       string   `json:"device_id"`
	Name           string   `json:"name"`
	Command        string   `json:"command"`
	Args           []string `json:"args,omitempty"`
	Cwd            string   `json:"cwd"`
	Status         string   `json:"status"` // starting|running|detached|exited|failed|terminated
	PID            int      `json:"pid,omitempty"`
	ExitCode       *int     `json:"exit_code,omitempty"`
	Cols           uint16   `json:"cols"`
	Rows           uint16   `json:"rows"`
	CreatedAt      int64    `json:"created_at"`
	StartedAt      *int64   `json:"started_at,omitempty"`
	EndedAt        *int64   `json:"ended_at,omitempty"`
	LastAttachedAt *int64   `json:"last_attached_at,omitempty"`
	// BufferSeqFrom/To 让前端在 attach 前就知道 ring buffer 里还有多少可用，
	// 从而判断自己断线期间丢了多少（§15.2）。
	BufferSeqFrom uint64 `json:"buffer_seq_from"`
	BufferSeqTo   uint64 `json:"buffer_seq_to"`
}

// HeartbeatPayload 是 Agent 的周期上报，同时充当 session 对账数据源。
type HeartbeatPayload struct {
	Sessions []HeartbeatSession `json:"sessions"`
	Update AgentUpdateStatus `json:"update"`
	Commands []CommandAvailability `json:"commands,omitempty"`
}

// AgentUpdateStatus reports only observable Agent state, never a guessed
// install outcome from the published Server version.
type AgentUpdateStatus struct {
	Enabled bool `json:"enabled"`
	State string `json:"state"`
	Version string `json:"version,omitempty"`
	Detail string `json:"detail,omitempty"`
	CheckedAt int64 `json:"checked_at,omitempty"`
	LastFailure *AgentUpdateFailure `json:"last_failure,omitempty"`
}

type AgentUpdateFailure struct {
	Version string `json:"version"`
	Reason string `json:"reason"`
	OccurredAt int64 `json:"occurred_at"`
}

// CommandAvailability is a fixed, display-only inventory. The Agent still
// checks allowed_commands when creating a session; a detected binary does not
// grant execution rights.
type CommandAvailability struct {
	ID string `json:"id"`
	Label string `json:"label"`
	Kind string `json:"kind"`
	Installed bool `json:"installed"`
	Allowed bool `json:"allowed"`
	Resume bool `json:"resume,omitempty"`
	WebURL string `json:"web_url,omitempty"`
}

// HeartbeatSession 是心跳里的会话摘要（只放对账必需的字段，尽量小）。
type HeartbeatSession struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Attached  int    `json:"attached"` // 当前 attach 的客户端数（多客户端，§7.6）
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
	PID       int    `json:"pid,omitempty"`
}

// SessionSyncPayload 是 Agent 重连后上报的全量会话列表，用于 Server 对账（§7.4）。
type SessionSyncPayload struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionCreatePayload 是浏览器请求新建会话。
//
// CommandID 与 Command/Args 二选一：
//   - 走白名单时只传 CommandID
//   - 允许自定义命令时传 Command/Args（需 Agent 侧显式开启，§21.1）
type SessionCreatePayload struct {
	DeviceID  string   `json:"device_id"`
	Name      string   `json:"name,omitempty"`
	CommandID string   `json:"command_id,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	Cwd       string   `json:"cwd"`
	Cols      uint16   `json:"cols"`
	Rows      uint16   `json:"rows"`
	// Resume 表示"恢复上次对话"（§21.3）。
	// Agent 据此把 allowed_commands 里的 resume_args 追加到命令行。
	// CodeGate 不知道对话内容，只知道这个 CLI 支持 resume 标志。
	Resume bool `json:"resume,omitempty"`
}

// SessionCreatedPayload 是新建成功的结果。
type SessionCreatedPayload struct {
	Session SessionSummary `json:"session"`
}

// SessionListPayload 是请求会话列表。
type SessionListPayload struct {
	DeviceID string `json:"device_id,omitempty"` // 为空则列出本设备全部
}

// SessionListedPayload 是会话列表结果。
type SessionListedPayload struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionAttachPayload 是请求接管一个会话的终端。
//
// Since 的语义（§15.3）：
//
//	0            → 客户端没有历史，请把 ring buffer 里能给的都给
//	lastSeq      → 客户端已有到 lastSeq 的输出，只要它之后的
//	< BufferFrom → 落后太多，Agent 只给能给的，并在响应里如实告知
type SessionAttachPayload struct {
	SessionID string `json:"session_id"`
	Since     uint64 `json:"since"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// SessionAttachedPayload 是 attach 成功的结果。
type SessionAttachedPayload struct {
	Session SessionSummary `json:"session"`
	// SeqFrom/SeqTo 是本次重放覆盖的区间。若 SeqFrom > 请求的 Since，
	// 说明客户端落后太多、中间有丢帧，前端应清屏后按 SeqFrom 重放。
	SeqFrom uint64 `json:"seq_from"`
	SeqTo   uint64 `json:"seq_to"`
	// Role 是本客户端在这次 attach 里拿到的角色（§7.6）。
	Role string `json:"role"` // controller | viewer
}

// SessionDetachPayload 是请求断开接管。
type SessionDetachPayload struct {
	SessionID string `json:"session_id"`
	AttachID string `json:"attach_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// SessionDetachedPayload 通知某客户端已被解除接管。
type SessionDetachedPayload struct {
	SessionID string `json:"session_id"`
	// Reason: client_close | superseded | idle | server_shutdown
	Reason string `json:"reason"`
}

// SessionClosePayload 是请求关闭会话（终止进程）。
type SessionClosePayload struct {
	SessionID string `json:"session_id"`
	Force     bool   `json:"force,omitempty"`
}

// SessionClosedPayload 通知会话已关闭。
type SessionClosedPayload struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// SessionResizePayload 是调整终端尺寸。
type SessionResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// SessionSignalPayload 是向会话投递信号。
//
// Windows 上只有 "int" 有真实实现（写 0x03，§7 F4），其余返回 unsupported。
type SessionSignalPayload struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal"` // int | term | kill
}

// SessionExitPayload 通知会话进程已退出。
type SessionExitPayload struct {
	SessionID string `json:"session_id"`
	ExitCode  int    `json:"exit_code"`
	Reason    string `json:"reason,omitempty"` // exited | signal | pty_error
}

// SessionRoleChangedPayload 通知 controller 变更（§7.6）。
type SessionRoleChangedPayload struct {
	SessionID  string `json:"session_id"`
	Controller string `json:"controller"` // 新的主控客户端 connID
}

// ErrorPayload 是 error 消息的负载。
type ErrorPayload struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
}

// ---------------------------------------------------------------------------
// Workspace 文件传输（docs/PHASE0-FILE-TRANSPORT.md §4）
//
// 路径约定：**一律用相对 allowed root 的相对路径**，不用绝对路径。
// 理由：绝对路径会暴露用户名与盘符结构；而相对路径正好是后续所有 API 的
// 入参格式，前端不用做任何转换。
// ---------------------------------------------------------------------------

// FileListPayload 请求列目录。
type FileListPayload struct {
	SessionID string `json:"session_id"`
	Path string `json:"path"` // 空串 = 列根目录
}

// FileListedPayload 是目录内容。
type FileListedPayload struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileEntry 是目录里的一个条目。
type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"` // 相对路径
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // Unix ms
	Mode     string `json:"mode,omitempty"`
	Symlink  bool   `json:"symlink,omitempty"`
}

// FileStatPayload 请求单个文件的元信息。
type FileStatPayload struct {
	SessionID string `json:"session_id"`
	Path string `json:"path"`
}

// FileStatResultPayload 是文件元信息。
//
// IsBinary 与 Encoding 由 Agent 读前 64 KB 探测得出 ——
// 这是传输层必要的判断（决定"能不能预览"），
// **不做语法解析、不做高亮、不做格式化**（那些是前端的事）。
type FileStatResultPayload struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	IsDir    bool   `json:"is_dir"`
	Symlink  bool   `json:"symlink,omitempty"`
	IsBinary bool   `json:"is_binary,omitempty"`
	Encoding string `json:"encoding,omitempty"` // utf-8 | utf-16le | gbk | binary
}

// FileReadPayload 请求下载（或预览）一个文件。
//
// Length 为 0 表示读到文件末尾；预览时前端传 64 KB。
type FileReadPayload struct {
	SessionID string `json:"session_id"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// FileReadResultPayload 返回一个有界文件块。数据为 base64，前端按 offset 顺序拼接。
type FileReadResultPayload struct {
	Path string `json:"path"`
	Size int64 `json:"size"`
	Offset int64 `json:"offset"`
	Data string `json:"data"`
}

// FileReadBeginPayload 通知传输即将开始。
//
// 拿到 TransferID 之后，双方之间的 FILE_DATA 帧就用它做 StreamID。
type FileReadBeginPayload struct {
	TransferID string `json:"transfer_id"`
	Path       string `json:"path"`
	Size       int64  `json:"size"` // 本次传输的总字节数
	IsBinary   bool   `json:"is_binary"`
	Encoding   string `json:"encoding,omitempty"`
	Modified   int64  `json:"modified"`
}

// FileAckPayload 是流控确认（docs/PHASE0-FILE-TRANSPORT.md §5）。
//
// 发送方在途未确认的块数不得超过 window；收到 ack 后按已确认块数放开窗口。
// 上传时由 Agent 发（它是接收方），下载时由浏览器发。
type FileAckPayload struct {
	TransferID    string `json:"transfer_id"`
	ReceivedBytes int64  `json:"received_bytes"`
}

// FileWritePayload 请求上传。
type FileWritePayload struct {
	SessionID string `json:"session_id"`
	UploadID string `json:"upload_id"`
	Offset int64 `json:"offset"`
	Data string `json:"data"`
	Final bool `json:"final"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Overwrite 默认为 false：目标已存在时拒绝。
	// 远程界面误操作的概率远高于本地，多一次显式确认的成本很低。
	Overwrite bool `json:"overwrite,omitempty"`
}

// FileWriteReadyPayload 表示 Agent 已建好临时文件，可以开始发字节了。
type FileWriteReadyPayload struct {
	TransferID string `json:"transfer_id"`
	Path       string `json:"path"`
}

// FileWriteDonePayload 表示落盘完成（临时文件已原子改名为目标）。
type FileWriteDonePayload struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// FileCancelPayload 取消一次传输。
type FileCancelPayload struct {
	SessionID string `json:"session_id"`
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason,omitempty"`
}
