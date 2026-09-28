// Package device 是设备的业务层：列表、改名、解绑、绑定、在线状态。
//
// # 授权在这里收口
//
// 所有方法都要求传入 userID，并在内部校验设备归属。这是 §10.5「唯一规则」
// 的落地点之一：`device.user_id == current_user.id`。
//
// ★ 上层 handler **不允许**绕过本包直接查库取设备 ——
// 一旦允许，IDOR 就会从某个被遗忘的 handler 里长出来。
// 把授权和查询绑在一个函数里，是防 IDOR 最省心的做法。
package device

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jojo/codegate/internal/storage"
)

var (
	// ErrNotFound 表示设备不存在，**或存在但不属于该用户**。
	//
	// ★ 两种情况刻意合并成同一个错误：如果「不存在」和「不是你的」返回
	// 不同的错误码，攻击者就能拿一批 UUID 去探测哪些设备真实存在
	// （枚举出有效 device_id 本身就是信息泄露）。
	ErrNotFound = errors.New("device: 设备不存在或无权访问")

	// ErrEmptyName 表示设备名为空。
	ErrEmptyName = errors.New("device: 设备名不能为空")
	// ErrNameTooLong 表示设备名过长。
	ErrNameTooLong = errors.New("device: 设备名过长")
	// ErrAlreadyPaired 表示设备已绑定到其他账号。
	ErrAlreadyPaired = errors.New("device: 设备已被其他账号绑定")
)

// MaxNameLen 是设备显示名的长度上限（按字符计，不是字节）。
//
// 限制的理由不是存储，而是**展示**：设备名会出现在手机上的列表里，
// 过长的名字会把布局撑坏。
const MaxNameLen = 64

// OnlineChecker 判断设备当前是否有活跃的 Agent 连接。
//
// 用接口而不是直接依赖注册表，是为了让 device 包不反向依赖 server 包
// （server 要依赖 device，反向依赖会成环）。
type OnlineChecker interface {
	IsOnline(deviceID string) bool
}

// View 是设备对外的视图：持久化字段 + 实时在线状态。
//
// 内嵌 *storage.Device 而不是复制字段：设备字段较多，复制一份意味着
// 每次 schema 变动都要改两处，迟早会漏。
type View struct {
	*storage.Device
	// Online 来自实时注册表，**不落库**。
	// 落库的话就需要一个后台任务去刷新，还会引入「DB 说在线但其实早断了」
	// 这种必然发生的状态不一致。
	Online bool
}

// ValidateName 校验设备名。
//
// 用 RuneCountInString 而不是 len：中文设备名「书房台式机」是 5 个字符
// 但 15 个字节，按字节算会误判为超长。
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ErrEmptyName
	}
	if utf8.RuneCountInString(trimmed) > MaxNameLen {
		return fmt.Errorf("%w: 最多 %d 个字符", ErrNameTooLong, MaxNameLen)
	}
	// 控制字符会让 UI 出现诡异的换行/覆盖，直接拒绝。
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("device: 设备名不能包含控制字符")
		}
	}
	return nil
}
