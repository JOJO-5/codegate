package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// 配对流程的超时。
const (
	// pairCodeTimeout 是等待 Server 下发配对码的上限。
	pairCodeTimeout = 30 * time.Second
	// pairCompleteSlack 是在配对码有效期之上额外等待的余量。
	//
	// 给余量是因为用户往往在最后一刻才输完：如果一到点就断连，
	// 用户看到的是"配对失败"，但他其实只差一秒 —— 而重新开始
	// 又要等一个新的配对码。多挂 30 秒的成本远低于这个体验损失。
	pairCompleteSlack = 30 * time.Second
)

// Pair 执行一次设备配对（§10.3）。
//
// 流程：
//
//	Agent 连上 Server
//	  → 发 agent.pair.begin（带 device_id + 公钥）
//	  → Server 回 agent.pair.code（配对码，形如 7F2K-93LM）
//	  → Agent 打印给用户
//	  → 用户在浏览器里输入配对码
//	  → Server 回 agent.pair.completed
//
// # 为什么配对码必须走「用户手工搬运」
//
// 这是整个系统里唯一一次人工介入，也是最关键的一环：它把
// 「网络上的某台机器」和「用户本人」绑定起来。
//
// 如果 Agent 能自动完成配对，那么任何一个能访问 Server 的程序
// 都能把自己注册成设备 —— 配对码的价值恰恰在于**只有能看到
// Agent 终端输出的人**才能完成它。
//
// 所以它打印在 Agent 自己的终端里（不是日志文件），且有效期很短。
func (a *Agent) Pair(ctx context.Context, out io.Writer) error {
	disp := newDispatcher(a.log)

	dialCtx, cancelDial := context.WithTimeout(ctx, DefaultConnectTimeout)
	conn, err := Dial(dialCtx, DialOptions{
		URL:      a.cfg.ServerURL,
		Insecure: a.cfg.Insecure,
		Log:      a.log,
		OnText:   disp.onText,
		OnBinary: disp.onBinary,
	})
	cancelDial()
	if err != nil {
		return err
	}
	defer conn.Close()

	platform, arch := DescribePlatform()
	begin, err := protocol.NewEnvelope(protocol.TypeAgentPairBegin, protocol.AgentPairBeginPayload{
		DeviceID:     a.id.DeviceID,
		PublicKey:    a.id.PublicKeyB64(),
		Name:         a.cfg.DeviceName,
		Platform:     platform,
		Arch:         arch,
		AgentVersion: Version,
	})
	if err != nil {
		return err
	}
	if err := sendEnvelope(conn, begin); err != nil {
		return fmt.Errorf("agent: 发送 pair.begin 失败: %w", err)
	}

	// ---- 等配对码 ----
	codeCtx, cancelCode := context.WithTimeout(ctx, pairCodeTimeout)
	codeEnv, err := disp.waitAuthAny(codeCtx, conn, protocol.TypeAgentPairCode, protocol.TypeError)
	cancelCode()
	if err != nil {
		return fmt.Errorf("agent: 等待配对码失败: %w", err)
	}
	if codeEnv.Type == protocol.TypeError {
		ep, _ := protocol.DecodePayload[protocol.ErrorPayload](codeEnv)
		return fmt.Errorf("agent: 配对被拒（%s）: %s", ep.Code, ep.Message)
	}
	code, err := protocol.DecodePayload[protocol.AgentPairCodePayload](codeEnv)
	if err != nil {
		return err
	}

	printPairCode(out, code)

	// ---- 等用户在浏览器里完成 ----
	wait := time.Duration(code.ExpiresIn)*time.Second + pairCompleteSlack
	doneCtx, cancelDone := context.WithTimeout(ctx, wait)
	doneEnv, err := disp.waitAuthAny(doneCtx, conn, protocol.TypeAgentPairCompleted, protocol.TypeError)
	cancelDone()
	if err != nil {
		return fmt.Errorf("agent: 等待配对完成失败（配对码可能已过期，重新运行 pair 可获取新的）: %w", err)
	}
	if doneEnv.Type == protocol.TypeError {
		ep, _ := protocol.DecodePayload[protocol.ErrorPayload](doneEnv)
		return fmt.Errorf("agent: 配对失败（%s）: %s", ep.Code, ep.Message)
	}
	done, err := protocol.DecodePayload[protocol.AgentPairCompletedPayload](doneEnv)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\n配对成功。\n")
	if done.UserEmail != "" {
		fmt.Fprintf(out, "  账号：%s\n", done.UserEmail)
	}
	fmt.Fprintf(out, "  设备：%s\n", a.cfg.DeviceName)
	fmt.Fprintf(out, "  设备 ID：%s\n\n", a.id.DeviceID)
	fmt.Fprintf(out, "下一步：运行 `codegate-agent run` 启动 Agent。\n")
	return nil
}

// printPairCode 把配对码打印成人一眼能读、手一打就准的格式。
//
// 刻意不写进日志文件：日志可能被采集、上传、共享，
// 而配对码是"能完成设备绑定"的凭据。它应该只出现在
// 用户正看着的那个终端里。
func printPairCode(out io.Writer, code protocol.AgentPairCodePayload) {
	const width = 46
	line := strings.Repeat("-", width)

	fmt.Fprintf(out, "\n%s\n", line)
	fmt.Fprintf(out, "  在浏览器里打开 CodeGate，输入下面的配对码：\n\n")
	fmt.Fprintf(out, "        %s\n\n", code.Code)
	if code.ExpiresIn > 0 {
		fmt.Fprintf(out, "  %s内有效。\n", humanDuration(time.Duration(code.ExpiresIn)*time.Second))
	}
	fmt.Fprintf(out, "%s\n\n", line)
}

// humanDuration 把时长写成中文可读形式。
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d >= time.Second:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	default:
		return d.String()
	}
}
