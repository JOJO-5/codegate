package protocol

import "fmt"

// Type 是控制消息的类型标识。命名约定：`<域>.<动作>`，全小写，点分隔。
type Type string

const (
	// ---- Agent 连接与鉴权 ----
	TypeAgentHello     Type = "agent.hello"
	TypeAgentChallenge Type = "agent.challenge"
	TypeAgentAuth      Type = "agent.auth"
	TypeAgentReady     Type = "agent.ready"
	TypeAgentHeartbeat Type = "agent.heartbeat"

	// ---- 设备配对 ----
	TypeAgentPairBegin     Type = "agent.pair.begin"
	TypeAgentPairCode      Type = "agent.pair.code"
	TypeAgentPairCompleted Type = "agent.pair.completed"

	// ---- 设备信息 ----
	TypeDeviceInfo Type = "device.info"

	// ---- 会话 ----
	TypeSessionSync     Type = "session.sync"
	TypeSessionCreate   Type = "session.create"
	TypeSessionCreated  Type = "session.created"
	TypeSessionList     Type = "session.list"
	TypeSessionListed   Type = "session.list.result"
	TypeSessionGet      Type = "session.get"
	TypeSessionInfo     Type = "session.info"
	TypeSessionAttach   Type = "session.attach"
	TypeSessionAttached Type = "session.attached"
	TypeSessionDetach   Type = "session.detach"
	TypeSessionDetached Type = "session.detached"
	TypeSessionClose    Type = "session.close"
	TypeSessionClosed   Type = "session.closed"
	TypeSessionResize   Type = "session.resize"
	TypeSessionSignal   Type = "session.signal"
	TypeSessionExit     Type = "session.exit"

	// ---- 多客户端（§7.6）----
	TypeSessionClaimControl Type = "session.claim_control"
	TypeSessionRoleChanged  Type = "session.role_changed"

	// ---- Workspace 文件传输（docs/PHASE0-FILE-TRANSPORT.md）----
	TypeFileList       Type = "file.list"
	TypeFileListed     Type = "file.list.result"
	TypeFileStat       Type = "file.stat"
	TypeFileStatResult Type = "file.stat.result"
	TypeFileRead       Type = "file.read"
	TypeFileReadBegin  Type = "file.read.begin"
	TypeFileReadResult Type = "file.read.result"
	TypeFileAck        Type = "file.ack"
	TypeFileWrite      Type = "file.write"
	TypeFileWriteReady Type = "file.write.ready"
	TypeFileWriteDone  Type = "file.write.done"
	TypeFileCancel     Type = "file.cancel"

	// DSH Web tunnel. The browser cannot emit these messages.
	TypeWebStart Type = "web.start"
	TypeWebStarted Type = "web.started"
	TypeWebOpen Type = "web.open"
	TypeWebClose Type = "web.close"
	TypeWebStop Type = "web.stop"
	TypeWebStopped Type = "web.stopped"

	// ---- 通用 ----
	TypeError Type = "error"
	TypePing  Type = "ping"
	TypePong  Type = "pong"
)

// Sender 标识消息的**发出方**。
//
// 注意区分两件事：
//   - Sender  = 谁发的（Agent / Client / Server）
//   - Side    = 这条连接是谁跟谁之间的（agent 连接 / client 连接）
//
// Server 在两个方向上都会发消息，所以不能用"连接侧"来表达发出方。
type Sender uint8

const (
	SentByAgent  Sender = 1 << iota // Agent 进程
	SentByClient                    // 浏览器
	SentByServer                    // Server
)

// Side 是一条连接的类别。
type Side uint8

const (
	SideAgentConn  Side = 1 << iota // Server ↔ Agent
	SideClientConn                  // Server ↔ 浏览器
)

func (s Side) String() string {
	switch s {
	case SideAgentConn:
		return "agent"
	case SideClientConn:
		return "client"
	default:
		return "unknown"
	}
}

// Peer 返回该连接上"对端"的身份。收到消息时用它来校验发出方。
func (s Side) Peer() Sender {
	switch s {
	case SideAgentConn:
		return SentByAgent
	case SideClientConn:
		return SentByClient
	default:
		return 0
	}
}

// Local 返回该连接上"本端"（即 Server 自己）的身份。
func (s Side) Local() Sender { return SentByServer }

// allowedSenders 声明每种消息允许由谁发出。
//
// ★ 这是一道安全闸门，不是文档。收到发出方不符的消息必须【直接断开连接】，
// 而不是忽略后继续。理由：浏览器若能发 `session.exit` 或 `agent.ready`，
// 就能伪造 Agent 事件，欺骗 UI 和 Server 的状态机。
var allowedSenders = map[Type]Sender{
	// ---- 只有 Agent 能发 ----
	TypeAgentHello:         SentByAgent,
	TypeAgentAuth:          SentByAgent,
	TypeAgentHeartbeat:     SentByAgent,
	TypeAgentPairBegin:     SentByAgent,
	TypeDeviceInfo:         SentByAgent,
	TypeSessionSync:        SentByAgent,
	TypeSessionCreated:     SentByAgent,
	TypeSessionListed:      SentByAgent,
	TypeSessionInfo:        SentByAgent,
	TypeSessionAttached:    SentByAgent,
	TypeSessionDetached:    SentByAgent,
	TypeSessionClosed:      SentByAgent,
	TypeSessionExit:        SentByAgent,
	TypeSessionRoleChanged: SentByAgent,
	TypeWebStarted: SentByAgent,
	TypeWebStopped: SentByAgent,

	// ---- 只有 Server 能发（对 Agent 的应答 / 对客户端的通知）----
	TypeAgentChallenge:     SentByServer,
	TypeAgentReady:         SentByServer,
	TypeAgentPairCode:      SentByServer,
	TypeAgentPairCompleted: SentByServer,
	TypeWebStart: SentByServer,
	TypeWebStop: SentByServer,
	TypeWebOpen: SentByServer,
	TypeWebClose: SentByServer | SentByAgent,

	// ---- 只有浏览器能发 ----
	TypeSessionCreate:       SentByClient,
	TypeSessionList:         SentByClient,
	TypeSessionGet:          SentByClient,
	TypeSessionAttach:       SentByClient,
	TypeSessionDetach:       SentByClient,
	TypeSessionClose:        SentByClient,
	TypeSessionResize:       SentByClient,
	TypeSessionSignal:       SentByClient,
	TypeSessionClaimControl: SentByClient,

	// ---- 文件传输：请求只有浏览器能发 ----
	TypeFileList:  SentByClient,
	TypeFileStat:  SentByClient,
	TypeFileRead:  SentByClient,
	TypeFileWrite: SentByClient,

	// ---- 文件传输：只有 Agent 能回 ----
	TypeFileListed:     SentByAgent,
	TypeFileStatResult: SentByAgent,
	TypeFileReadBegin:  SentByAgent,
	TypeFileReadResult: SentByAgent,
	TypeFileWriteReady: SentByAgent,
	TypeFileWriteDone:  SentByAgent,

	// ---- 双向 ----
	// file.ack：下载时由浏览器确认（它是接收方），上传时由 Agent 确认。
	// file.cancel：任一方都可以取消。
	TypeFileAck:    SentByAgent | SentByClient,
	TypeFileCancel: SentByAgent | SentByClient,
	TypeError:      SentByAgent | SentByClient | SentByServer,
	TypePing:       SentByAgent | SentByClient | SentByServer,
	TypePong:       SentByAgent | SentByClient | SentByServer,
}

// Valid 判断类型是否为本协议已知。
func Valid(t Type) bool {
	_, ok := allowedSenders[t]
	return ok
}

// AllowedSenders 返回允许发出该消息的身份集合。第二个返回值表示类型是否已知。
func AllowedSenders(t Type) (Sender, bool) {
	s, ok := allowedSenders[t]
	return s, ok
}

// CanSend 判断某个身份是否有权发出该类型的消息。
func CanSend(t Type, from Sender) bool {
	s, ok := allowedSenders[t]
	return ok && s&from != 0
}

// ValidateFrom 校验一条收到的消息是否符合其发出方身份。
//
// 未知类型、发出方不符，都返回 ErrInvalidMessage。
func ValidateFrom(t Type, from Sender) error {
	s, ok := allowedSenders[t]
	if !ok {
		return fmt.Errorf("%w: 未知消息类型 %q", ErrInvalidMessage, t)
	}
	if s&from == 0 {
		return fmt.Errorf("%w: 消息 %q 不允许由 %s 发出", ErrInvalidMessage, t, from)
	}
	return nil
}

// ValidateFromSide 是 ValidateFrom 的便捷封装：按连接侧自动推断对端身份。
func ValidateFromSide(t Type, side Side) error {
	return ValidateFrom(t, side.Peer())
}

func (s Sender) String() string {
	switch s {
	case SentByAgent:
		return "agent"
	case SentByClient:
		return "client"
	case SentByServer:
		return "server"
	default:
		return "unknown"
	}
}
