package device

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/storage"
)

// Service 是设备的业务入口。
//
// 无状态（除依赖外），可安全并发使用。
type Service struct {
	store  storage.Store
	online OnlineChecker
	log    *slog.Logger
	// now 可注入，便于测试时间相关逻辑。
	now func() time.Time
}

// alwaysOffline 是 online 为 nil 时的兜底实现。
//
// 让 nil 变成「全部离线」而不是 panic：设备列表页显示全离线，
// 比服务直接挂掉要好得多，而且这个状态是自解释的。
type alwaysOffline struct{}

func (alwaysOffline) IsOnline(string) bool { return false }

// NewService 构造设备服务。
//
// online 传 nil 时所有设备都报告为离线（用于不关心在线状态的场景，
// 例如 CLI 管理命令）。
func NewService(store storage.Store, online OnlineChecker, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if online == nil {
		online = alwaysOffline{}
	}
	return &Service{store: store, online: online, log: log, now: time.Now}
}

// SetClock 替换时间源。仅供测试使用。
func (s *Service) SetClock(fn func() time.Time) { s.now = fn }

// List 列出某账号的全部设备，附带实时在线状态。
func (s *Service) List(ctx context.Context, userID string) ([]View, error) {
	devices, err := s.store.DevicesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 预分配成非 nil 切片：JSON 序列化时 `nil` 会变成 `null` 而不是 `[]`，
	// 前端处理 null 和空数组要写两套分支。
	out := make([]View, 0, len(devices))
	for _, d := range devices {
		out = append(out, View{Device: d, Online: s.online.IsOnline(d.ID)})
	}
	return out, nil
}

// Get 取单个设备（含归属校验）。
func (s *Service) Get(ctx context.Context, userID, deviceID string) (*View, error) {
	d, err := s.owned(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}
	return &View{Device: d, Online: s.online.IsOnline(d.ID)}, nil
}

// Rename 改设备显示名。
func (s *Service) Rename(ctx context.Context, userID, deviceID, name string) (*View, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if _, err := s.owned(ctx, userID, deviceID); err != nil {
		return nil, err
	}
	if err := s.store.RenameDevice(ctx, deviceID, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, deviceID)
}

// Delete 解绑并删除设备（其会话记录由外键级联清除）。
//
// 调用方（handler）在删除后还需要踢掉该设备当前的 Agent 连接 ——
// 那是连接层的职责，不在这里做（device 包不认识 WebSocket）。
func (s *Service) Delete(ctx context.Context, userID, deviceID string) error {
	if _, err := s.owned(ctx, userID, deviceID); err != nil {
		return err
	}
	return s.store.DeleteDevice(ctx, deviceID)
}

// Bind 把设备绑定到账号。pairing 确认流程调用。
//
// ★ 不校验归属，因为此时设备**还没有** owner —— 这正是绑定的含义。
// 授权发生在调用方：必须先验过配对码（证明请求者持有 Agent 出示的 code）。
func (s *Service) Bind(ctx context.Context, userID, deviceID string) error {
	err := s.store.BindDevice(ctx, deviceID, userID, s.now())
	if errors.Is(err, storage.ErrConflict) {
		// 翻译成领域错误，让上层不必认识 storage 的哨兵。
		return ErrAlreadyPaired
	}
	return err
}

// MarkSeen 更新设备的最后在线时间。
//
// 不校验归属：Agent 在认证通过之前就会调它（更新 last_seen 是
// 连接建立时的副作用），此时还不知道 owner。
func (s *Service) MarkSeen(ctx context.Context, deviceID string) error {
	return s.store.TouchDeviceLastSeen(ctx, deviceID, s.now())
}

// owned 取出设备并校验它属于该用户。**本包所有读写操作都必须经过它。**
//
// 返回的错误刻意与「设备不存在」完全一致（见 ErrNotFound 的注释）。
func (s *Service) owned(ctx context.Context, userID, deviceID string) (*storage.Device, error) {
	d, err := s.store.DeviceByID(ctx, deviceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if d.UserID != userID {
		// 记 Warn 而不是 Debug：正常的跨账号访问是**不应该发生**的，
		// 出现就说明要么前端有 bug，要么有人在扫 ID。两者都值得看见。
		s.log.Warn("拒绝跨账号访问设备",
			"user_id", userID,
			"device_id", deviceID,
			"owner_id", d.UserID,
		)
		return nil, ErrNotFound
	}
	return d, nil
}
