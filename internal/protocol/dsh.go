package protocol

const (
	CodeDSHPortInUse       ErrorCode = "dsh_port_in_use"
	CodeDSHRestartRequired ErrorCode = "dsh_restart_required"
	CodeDSHUnhealthy       ErrorCode = "dsh_unhealthy"
	CodeDSHNotInstalled    ErrorCode = "dsh_not_installed"
	CodeDSHStartFailed     ErrorCode = "dsh_start_failed"
	CodeDSHCleanupFailed   ErrorCode = "dsh_cleanup_failed"
)

// DSHStartError returns fixed, credential-free messages across the Agent/Server
// boundary. Never forward arbitrary CLI stderr or Agent-supplied messages.
func DSHStartError(code ErrorCode) *CodeError {
	switch code {
	case CodeDSHPortInUse:
		return NewError(code, "电脑的 3080 端口已被其他程序占用。请确认该程序没有任务后，在电脑上停止它，再重新连接；CodeGate 不会自动结束外部进程。")
	case CodeDSHRestartRequired:
		return NewError(code, "现有 DSH Web 的访问地址与当前配置不同。不会自动重启；请确认没有运行任务后，显式停止 DSH Web，再打开。")
	case CodeDSHUnhealthy:
		return NewError(code, "现有 DSH Web 未通过连接检查，不会自动重启。请先核对电脑上的运行任务，再选择停止并重新打开。")
	case CodeDSHNotInstalled:
		return NewError(code, "Agent 服务账户未找到 dsh。请在目标电脑安装并确认服务账户可访问后重试。")
	case CodeDSHStartFailed:
		return NewError(code, "DSH Web 未能完成启动或本机认证。请在电脑检查 DSH 安装、配置及端口后重试。")
	case CodeDSHCleanupFailed:
		return NewError(code, "本次 DSH 启动失败，进程停止尚未确认。请在电脑核对该实例后重试，CodeGate 不会接管其他进程。")
	default:
		return NewError(CodeInternal, "DSH Web 启动失败；旧版 Agent 可能未提供具体原因，请检查电脑上的 DSH 安装与端口。")
	}
}
