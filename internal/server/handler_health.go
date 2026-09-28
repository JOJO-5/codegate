package server

import (
	"context"
	"net/http"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// Version 是 Server 的构建版本，由 cmd/codegate-server 在启动时覆盖
// （或构建期用 -ldflags "-X .../internal/server.Version=v1.2.3" 注入）。
//
// 用包级变量而不是常量：常量没法被 ldflags 覆盖，而版本号恰恰是
// 最需要在构建流水线里注入、最不该出现在源码里的东西 ——
// 写在源码里就一定会忘记改。
var Version = "dev"

// readyPingTimeout 是 /readyz 里 DB ping 的超时。
//
// 探针**必须**有超时：没有超时的话，DB 卡住时 /readyz 会一直挂着，
// 而负载均衡器看到的是「探针没返回」而不是「不健康」，
// 于是继续往这台机器上打流量。
const readyPingTimeout = 2 * time.Second

// handleHealthz 是存活探针（liveness）。
//
// ★ 刻意不做任何 I/O。
//
// 存活探针回答的是「这个进程还该不该活着」。如果它去 ping 数据库，
// 那么数据库抖一下，编排系统就会认为进程死了并把它杀掉重启 ——
// 重启并不会让数据库恢复，却会额外丢掉所有活跃的 WS 连接。
// 「依赖不健康」是就绪探针（/readyz）该回答的问题。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": int64(s.Uptime().Seconds()),
	})
}

// handleReadyz 是就绪探针（readiness）：能不能开始接流量。
//
// 与 /healthz 的分工：编排系统用 /readyz 决定「要不要把我加进后端列表」，
// 用 /healthz 决定「要不要重启我」。两者混用是很多线上事故的源头。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		// 把底层错误记进日志，但**不回给探针调用方** ——
		// 探针端点通常无认证，回显 SQL 错误等于给外部送情报。
		s.log.Warn("就绪探针失败：数据库不可达", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"reason": "database",
		})
		return
	}

	stats := s.relay.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ready",
		"uptime":         int64(s.Uptime().Seconds()),
		"agents":         s.reg.AgentCount(),
		"clients":        s.reg.ClientCount(),
		"routed_frames":  stats.RoutedFrames,
		"dropped_frames": stats.DroppedFrames,
	})
}

// handleVersion 返回 Server 版本与协议版本范围。
//
// 无认证，但**信息量刻意很小**：协议版本本来就要在握手时交换，
// 版本号则用于让前端提示「服务端太旧，请升级」。
// 不含构建时间、commit、Go 版本这些 —— 它们能拼出精确的漏洞窗口。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  Version,
		"protocol": protocol.Current(),
	})
}
