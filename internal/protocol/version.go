// Package protocol 定义 CodeGate 的线协议：控制消息信封 + 二进制终端帧。
//
// 这个包被 Server 和 Agent 共享，因此它只允许包含**纯数据结构和编解码**，
// 不允许出现任何业务逻辑、I/O、日志、全局状态。它是整个项目里最该保持
// 无聊的一个包 —— 无聊意味着可靠。
//
// 设计文档：docs/PHASE0-ARCHITECTURE.md §9 §16
package protocol

import "fmt"

// 协议版本。
//
// 递增规则（§9.4）：
//   - 新增**可选**字段 → 不动版本号（向后兼容）
//   - 删除字段、改变语义、改变二进制帧布局 → 递增
//
// Server 必须能同时服务相邻两个版本的 Agent，至少一个发布周期。
const (
	Version1 uint8 = 1

	// MinSupported / MaxSupported 是本端能理解的范围。
	MinSupported uint8 = Version1
	MaxSupported uint8 = Version1
)

// VersionInfo 是握手时交换的版本声明。
type VersionInfo struct {
	Min uint8 `json:"min"`
	Max uint8 `json:"max"`
}

// Current 返回本端支持的版本范围。
func Current() VersionInfo {
	return VersionInfo{Min: MinSupported, Max: MaxSupported}
}

// Compatible 判断对端声明的范围与本端是否有交集。
func Compatible(peer VersionInfo) bool {
	return peer.Max >= MinSupported && peer.Min <= MaxSupported
}

// Negotiate 选出一个双方都支持的最高版本。
//
// 不做静默降级：不兼容时返回错误，由调用方明确告知用户去升级（§9.4）。
func Negotiate(peer VersionInfo) (uint8, error) {
	if !Compatible(peer) {
		return 0, fmt.Errorf(
			"%w: 对端支持 %d-%d，本端支持 %d-%d",
			ErrVersionMismatch, peer.Min, peer.Max, MinSupported, MaxSupported,
		)
	}
	return min(peer.Max, MaxSupported), nil
}
