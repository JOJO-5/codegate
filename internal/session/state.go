package session

// ProcState 是会话在**进程维度**上的状态。
type ProcState uint8

const (
	// ProcStarting 表示 PTY 正在创建。
	ProcStarting ProcState = iota
	// ProcRunning 表示进程活着。
	ProcRunning
	// ProcExited 表示进程正常结束（含非 0 退出码）。
	ProcExited
	// ProcFailed 表示 PTY 创建失败或进程异常终止。
	ProcFailed
	// ProcTerminated 表示被用户显式关闭。
	ProcTerminated
)

func (s ProcState) String() string {
	switch s {
	case ProcStarting:
		return "starting"
	case ProcRunning:
		return "running"
	case ProcExited:
		return "exited"
	case ProcFailed:
		return "failed"
	case ProcTerminated:
		return "terminated"
	default:
		return "unknown"
	}
}

// Alive 表示进程还在跑。
func (s ProcState) Alive() bool { return s == ProcStarting || s == ProcRunning }

// AttachState 是会话在**有没有人看着**这个维度上的状态。
type AttachState uint8

const (
	// AttachNone 表示没有客户端 attach（detached）。
	AttachNone AttachState = iota
	// AttachAttached 表示至少有一个客户端 attach。
	AttachAttached
)

func (s AttachState) String() string {
	if s == AttachAttached {
		return "attached"
	}
	return "detached"
}

// Status 是对外暴露的扁平状态。
//
// ★ 它与规范要求的 6 个值完全一致 —— 内部的"两轴模型"是实现细节，
// 不泄漏到协议和数据库里。
type Status string

const (
	StatusStarting   Status = "starting"
	StatusRunning    Status = "running"
	StatusDetached   Status = "detached"
	StatusExited     Status = "exited"
	StatusFailed     Status = "failed"
	StatusTerminated Status = "terminated"
)

// Derive 把内部两个正交维度映射成对外的扁平状态。
//
// 为什么要两轴而不是一个枚举（§7.1）：
// 「running」和「detached」根本不在同一个维度上 —— 前者是"进程活着"，
// 后者是"有人看着"。把它们并列会导致状态机不自洽：
// 「detached 且已退出」该填哪个？
//
// 拆成两轴后，状态机自洽；对外仍然只暴露那 6 个值，API 完全兼容。
func Derive(p ProcState, a AttachState) Status {
	switch p {
	case ProcStarting:
		return StatusStarting
	case ProcRunning:
		if a == AttachAttached {
			return StatusRunning
		}
		return StatusDetached
	case ProcExited:
		return StatusExited
	case ProcFailed:
		return StatusFailed
	case ProcTerminated:
		return StatusTerminated
	default:
		return StatusFailed
	}
}

// CanAttach 判断某个状态下的会话还允许被 attach。
//
// 允许 attach 到已退出的会话：用户点错了或者想回看最后那段输出时，
// 直接报错是很差的体验（不变量 I3）。
// 只有"已被用户关闭"的会话才彻底拒绝。
func CanAttach(p ProcState) bool {
	return p != ProcTerminated
}

// CanInput 判断某个状态下的会话还接受 stdin。
func CanInput(p ProcState) bool {
	return p == ProcRunning
}
