package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func (a *Agent) onLiveContinuationCheck(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.TaskRunPayload](req)
	result := protocol.TaskStatePayload{State: "needs_review"}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = a.checkLiveContinuation(ctx, p)
	}
	if err != nil {
		result.Detail = err.Error()
	} else {
		result.State = "ready"
	}
	a.reply(req, protocol.TypeTaskState, result)
}

// Read-only preflight: no receipt, thread/resume, turn/start or daemon startup.
// A reachable service alone does not prove it owns the user's running thread.
func (a *Agent) checkLiveContinuation(ctx context.Context, p protocol.TaskRunPayload) error {
	if p.Mode != "live_quota" || p.UserID == "" || p.Source.Recovery == nil || p.Source.Recovery.NativeID == "" {
		return errors.New("自动续跑配置无效")
	}
	sess, ok := a.mgr.Get(parseUUID(p.Source.SessionID))
	if !ok || !isRecoverableLive(sess.Summary()) {
		return errors.New("原终端已结束，请恢复原对话后重新开启自动续跑")
	}
	current := sess.Summary()
	revision := sess.InputRevision()
	if (sess.UserID != uuid.Nil && sess.UserID.String() != p.UserID) || !sameNativeTask(current, p.Source) || !supportedLiveTool(current.Command, current.Args) {
		return errors.New("原终端与原生对话身份不一致，未开启自动续跑")
	}
	if _, _, err := a.recoveryCommand(current); err != nil {
		return errors.New("原 CLI 命令授权已变更，未开启自动续跑")
	}
	if _, err := a.ws.Resolve(current.Cwd); err != nil {
		return errors.New("工作目录已不在授权范围")
	}
	if quotaProvider(current.Command, current.Args) == "codex" {
		for i, arg := range current.Args {
			if arg == "--no-daemon" {
				return errors.New("此 Codex 终端使用独立进程，无法通过共享接口续跑；请结束原终端，再使用普通 Codex 命令恢复原对话")
			}
			if strings.HasPrefix(arg, "--remote=") && arg != "--remote=unix://" || arg == "--remote" && (i+1 >= len(current.Args) || current.Args[i+1] != "unix://") {
				return errors.New("此 Codex 终端使用自定义控制地址，暂不支持自动续跑；原终端和对话保留")
			}
		}
	}
	finish, err := a.beginActivity()
	if err != nil {
		return errors.New("Agent 正在更新，请稍后重试")
	}
	defer finish()
	control, err := a.openLiveControl(ctx, current, BuildEnv(a.cfg, 80, 24), p)
	if err != nil {
		var unavailable *codexControlUnavailable
		if errors.As(err, &unavailable) {
			return errors.New("尚未开启：Codex 本机控制服务不可连接。请在本机运行 codex app-server daemon start 后重试；若原对话仍无法监测，请结束原终端，再恢复原对话。项目文件和对话保留")
		}
		return err
	}
	defer control.close()
	thread, _, err := control.readThread(current.Recovery.NativeID)
	if err != nil {
		return err
	}
	if thread.ID != current.Recovery.NativeID || !sameDirectory(thread.Cwd, current.Cwd) || thread.Status.Type == "notLoaded" {
		return errors.New("尚未开启：原对话未加载到当前控制服务。请结束原终端，再恢复原对话后重试；项目文件和对话保留")
	}
	if !isRecoverableLive(sess.Summary()) || !sameNativeTask(sess.Summary(), current) || sess.InputRevision() != revision {
		return errors.New("核对期间原终端已有新输入、结束或切换对话，请重新打开自动续跑")
	}
	return nil
}
