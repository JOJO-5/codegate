// Package logging 统一日志初始化与敏感字段脱敏。
//
// 这个包存在的唯一理由是**安全门槛 C2**：
//
//	grep -riE "(password|token|private_key|stdin)" 在日志里不得命中敏感值
//
// 靠「写日志时小心点」是做不到的 —— 总有人会 `log.Info("login", "req", req)`。
// 所以脱敏做在 handler 层，对所有日志调用**强制生效**，绕不过去。
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Options 控制日志输出形态。
type Options struct {
	Level string    // debug | info | warn | error
	Out   io.Writer // 为空则用 os.Stderr
	// Text 输出人类可读格式（开发用）。默认 JSON（生产用，便于采集）。
	Text bool
	// AddSource 是否记录调用位置。开发时有用，生产会显著增加体积。
	AddSource bool
}

// New 构造一个已挂载脱敏 handler 的 logger。
func New(o Options) *slog.Logger {
	if o.Out == nil {
		o.Out = os.Stderr
	}
	opts := &slog.HandlerOptions{
		Level:     ParseLevel(o.Level),
		AddSource: o.AddSource,
	}
	var base slog.Handler
	if o.Text {
		base = slog.NewTextHandler(o.Out, opts)
	} else {
		base = slog.NewJSONHandler(o.Out, opts)
	}
	return slog.New(&redactHandler{next: base})
}

// Setup 构造 logger 并设为全局默认。
func Setup(o Options) *slog.Logger {
	l := New(o)
	slog.SetDefault(l)
	return l
}

// ParseLevel 把字符串转成 slog.Level。无法识别时返回 info ——
// 配置校验已经拦过非法值，这里只是不让程序因为一个日志级别崩掉。
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redactHandler 在写出前对每条记录做脱敏。
type redactHandler struct {
	next slog.Handler
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(RedactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		safe[i] = RedactAttr(a)
	}
	return &redactHandler{next: h.next.WithAttrs(safe)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{next: h.next.WithGroup(name)}
}
