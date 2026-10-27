package server

import "github.com/jojo/codegate/internal/protocol"

func validateNativeLaunchMode(caps protocol.AgentCaps, commands []protocol.CommandAvailability, req protocol.SessionCreatePayload) error {
	if req.LaunchMode == "" {
		return nil
	}
	if !caps.NativeLaunchModes {
		return protocol.NewError(protocol.CodeInvalidPayload, "此 Agent 不支持原生启动模式预选，请更新 Agent 或选择继承本机设置")
	}
	if req.CommandID != "" && req.Command == "" && len(req.Args) == 0 && !req.Resume {
		for _, command := range commands {
			if command.ID != req.CommandID || !command.Allowed || !command.Installed {
				continue
			}
			for _, mode := range command.NativeLaunchModes {
				if mode.ID == req.LaunchMode {
					return nil
				}
			}
		}
	}
	return protocol.NewError(protocol.CodeInvalidPayload, "此命令不支持所选原生模式；请刷新工具列表后重新选择")
}
