package agent

import (
	"context"
	"log/slog"
	"time"
)

// heartbeatTicker 周期性地执行一次回调（§3.3）。
//
// # 它不只是"保活"
//
// Agent 的心跳同时充当 **session 对账的数据源**（§7.4）：心跳里带着
// 本机全部会话的摘要，Server 用它发现两种不一致 ——
//
//   - Server 记录了某个会话，Agent 的摘要里没有 → 那个会话已经死了
//   - Agent 的摘要里有某个会话，Server 没记录 → Server 重启后丢了状态
//
// 所以心跳**不能省**，也不能"连上就不发了"。它是分布式状态收敛的
// 唯一周期信号。
type heartbeatTicker struct {
	interval time.Duration
	onTick   func(ctx context.Context) error
	log      *slog.Logger
}

func newHeartbeatTicker(interval time.Duration, log *slog.Logger, onTick func(context.Context) error) *heartbeatTicker {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &heartbeatTicker{interval: interval, onTick: onTick, log: log}
}

// run 阻塞运行直到 ctx 结束。
func (h *heartbeatTicker) run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()

	h.log.Debug("心跳已启动", "interval", h.interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// 心跳失败**不终止连接**：发送失败通常意味着链路已经出问题，
			// 而 readPump 会更快地发现（读超时 / 写失败）。
			// 在这里主动断连只会让两条路径互相干扰，日志也更难读。
			if err := h.onTick(ctx); err != nil {
				h.log.Warn("心跳发送失败", "err", err)
			}
		}
	}
}
