package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig 返回一份能通过 Validate 的配置。
//
// 用 t.TempDir() 而不是写死 "/srv/work"：Windows 上 `filepath.IsAbs("/srv/work")`
// 返回 **false**（那是 Unix 风格的路径，没有盘符），写死会让测试在
// Windows 上以"看起来是代码 bug"的方式失败。
func validConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.ServerURL = "wss://example.com/ws/agent"
	c.AllowedRoots = []string{t.TempDir()}
	c.AllowedCommands = []CommandSpec{
		{ID: "shell", Label: "Shell", Command: "cmd.exe", Kind: KindShell},
		{ID: "claude", Label: "Claude Code", Command: "claude", Kind: KindTUI,
			ResumeArgs: []string{"--continue"}},
	}
	return c
}

func TestValidateAcceptsMinimal(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("最小可用配置没有通过校验: %v", err)
	}
}

func TestValidateRejectsMissingServerURL(t *testing.T) {
	c := validConfig(t)
	c.ServerURL = ""
	if err := c.Validate(); err == nil {
		t.Error("缺少 server_url 却通过了校验")
	}
}

// TestValidateRejectsPlaintextWS 是安全默认值的回归测试：
// 不加 insecure 就不该允许明文 ws://。
func TestValidateRejectsPlaintextWS(t *testing.T) {
	c := validConfig(t)
	c.ServerURL = "ws://example.com/ws/agent"
	c.Insecure = false

	err := c.Validate()
	if err == nil {
		t.Fatal("ws:// 在没有显式 insecure 的情况下通过了校验")
	}
	if !strings.Contains(err.Error(), "wss") {
		t.Errorf("错误信息没有指出该怎么修（应提到 wss）: %v", err)
	}

	c.Insecure = true
	if err := c.Validate(); err != nil {
		t.Errorf("显式设置 insecure 后 ws:// 应当被允许: %v", err)
	}
}

func TestValidateRejectsUnknownScheme(t *testing.T) {
	c := validConfig(t)
	c.ServerURL = "ftp://example.com/x"
	c.Insecure = true
	if err := c.Validate(); err == nil {
		t.Error("未知协议通过了校验")
	}
}

func TestValidateRejectsEmptyRoots(t *testing.T) {
	c := validConfig(t)
	c.AllowedRoots = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("空的 allowed_roots 通过了校验")
	}
	// 这条错误信息很重要：它要能解释"为什么不能给个默认值"。
	if !strings.Contains(err.Error(), "整个磁盘") {
		t.Errorf("错误信息没有解释清楚风险: %v", err)
	}
}

func TestValidateRejectsRelativeRoot(t *testing.T) {
	c := validConfig(t)
	c.AllowedRoots = []string{"relative/path"}
	if err := c.Validate(); err == nil {
		t.Error("相对路径的 allowed_roots 通过了校验")
	}
}

func TestValidateAllowsEmptyCommandListForWebApproval(t *testing.T) {
	c := validConfig(t)
	c.AllowedCommands = nil
	c.AllowCustomCommands = false
	if err := c.Validate(); err != nil {
		t.Errorf("空白名单应允许设备页授权固定工具: %v", err)
	}

	// 开了自定义命令就应该允许没有白名单。
	c.AllowCustomCommands = true
	if err := c.Validate(); err != nil {
		t.Errorf("开启 allow_custom_commands 后应当允许空白名单: %v", err)
	}
}

func TestValidateRejectsDuplicateCommandID(t *testing.T) {
	c := validConfig(t)
	c.AllowedCommands = append(c.AllowedCommands, CommandSpec{
		ID: "shell", Label: "重复", Command: "bash", Kind: KindShell,
	})
	if err := c.Validate(); err == nil {
		t.Error("重复的命令 ID 通过了校验")
	}
}

// TestValidateRejectsBadKind 钉住 P2 引入的 kind 字段。
//
// kind 决定前端能否提供 Ctrl+C（架构 §6.5），写错了不会崩，
// 但会让用户在 shell 会话里点一个会破坏会话的按钮。
// 所以它必须在校验期就拦住，不能等到运行时"未知就按 shell 处理"。
func TestValidateRejectsBadKind(t *testing.T) {
	c := validConfig(t)
	c.AllowedCommands[0].Kind = ""

	err := c.Validate()
	if err == nil {
		t.Fatal("空的 command kind 通过了校验")
	}
	if !strings.Contains(err.Error(), "Ctrl+C") {
		t.Errorf("错误信息没有说明 kind 的用途: %v", err)
	}
}

func TestValidateRejectsNonPositiveLimits(t *testing.T) {
	c := validConfig(t)
	c.HeartbeatInterval = 0
	if err := c.Validate(); err == nil {
		t.Error("heartbeat_interval=0 通过了校验")
	}

	c = validConfig(t)
	c.MaxSessions = -1
	if err := c.Validate(); err == nil {
		t.Error("max_sessions=-1 通过了校验")
	}
}

// ---------------------------------------------------------------------------
// Duration
// ---------------------------------------------------------------------------

func TestDurationJSONRoundTrip(t *testing.T) {
	type wrapper struct {
		D Duration `json:"d"`
	}

	for _, want := range []time.Duration{
		20 * time.Second,
		1500 * time.Millisecond,
		2 * time.Minute,
	} {
		data, err := json.Marshal(wrapper{D: Duration(want)})
		if err != nil {
			t.Fatalf("序列化 %v 失败: %v", want, err)
		}
		// 必须是可读的字符串形式，不是纳秒整数。
		if !strings.Contains(string(data), `"`) {
			t.Errorf("Duration 被序列化成了 %s，期望 \"20s\" 这样的字符串", data)
		}

		var got wrapper
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("反序列化 %s 失败: %v", data, err)
		}
		if got.D.Std() != want {
			t.Errorf("往返后 = %v，期望 %v", got.D.Std(), want)
		}
	}
}

func TestDurationAcceptsNanos(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte("5000000000"), &d); err != nil {
		t.Fatalf("裸纳秒数被拒了: %v", err)
	}
	if d.Std() != 5*time.Second {
		t.Errorf("= %v，期望 5s", d.Std())
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Error("非法时长字符串被接受了")
	}
}

// ---------------------------------------------------------------------------
// Prepare
// ---------------------------------------------------------------------------

func TestPrepareFillsDefaults(t *testing.T) {
	c := validConfig(t)
	c.MaxSessions = 0
	c.HeartbeatInterval = 0
	c.StateDir = ""

	got, err := c.Prepare()
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	if got.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d，期望 %d", got.MaxSessions, DefaultMaxSessions)
	}
	if got.HeartbeatInterval.Std() != DefaultHeartbeatInterval {
		t.Errorf("HeartbeatInterval = %v，期望 %v", got.HeartbeatInterval, DefaultHeartbeatInterval)
	}
	if got.StateDir == "" {
		t.Error("StateDir 没有被填上默认值")
	}
}

func TestPrepareDeduplicatesRoots(t *testing.T) {
	root := t.TempDir()
	c := validConfig(t)
	c.AllowedRoots = []string{root, root, filepath.Join(root, ".")}

	got, err := c.Prepare()
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	if len(got.AllowedRoots) != 1 {
		t.Errorf("去重后应剩 1 个根，实际 %d 个: %v", len(got.AllowedRoots), got.AllowedRoots)
	}
}

func TestPrepareKeepsReconnectMaxAboveMin(t *testing.T) {
	c := validConfig(t)
	c.ReconnectMin = Duration(30 * time.Second)
	c.ReconnectMax = Duration(time.Second) // 故意写反

	got, err := c.Prepare()
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	// 上限小于下限会让退避算出递减的间隔 —— 必须被纠正。
	if got.ReconnectMax < got.ReconnectMin {
		t.Errorf("ReconnectMax(%v) 仍小于 ReconnectMin(%v)",
			got.ReconnectMax.Std(), got.ReconnectMin.Std())
	}
}

func TestFindCommand(t *testing.T) {
	c := validConfig(t)

	if spec, ok := c.FindCommand("claude"); !ok || spec.Kind != KindTUI {
		t.Errorf("找不到 claude 或 kind 不对: %+v ok=%v", spec, ok)
	}
	if _, ok := c.FindCommand("nope"); ok {
		t.Error("不存在的命令 ID 居然被找到了")
	}
}

// ---------------------------------------------------------------------------
// 加载与覆盖
// ---------------------------------------------------------------------------

func TestLoadMissingFileReturnsDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("配置文件不存在时应当返回默认值而不是错误: %v", err)
	}
	if cfg.ServerURL != "" {
		t.Errorf("默认配置不该有 server_url，实际 %q", cfg.ServerURL)
	}
}

func TestLoadInvalidFileReturnsError(t *testing.T) {
	// 配置存在但坏了必须报错：静默退回默认值会让用户以为配置生效了。
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("损坏的配置文件被静默忽略了")
	}
}

func TestApplyEnv(t *testing.T) {
	c := DefaultConfig()
	env := map[string]string{
		"CODEGATE_SERVER_URL":  "wss://env.example.com/ws/agent",
		"CODEGATE_DEVICE_NAME": "from-env",
		"CODEGATE_INSECURE":    "true",
	}
	c.ApplyEnv(func(k string) string { return env[k] })

	if c.ServerURL != env["CODEGATE_SERVER_URL"] {
		t.Errorf("ServerURL = %q，期望 %q", c.ServerURL, env["CODEGATE_SERVER_URL"])
	}
	if c.DeviceName != "from-env" {
		t.Errorf("DeviceName = %q", c.DeviceName)
	}
	if !c.Insecure {
		t.Error("Insecure 没有被环境变量打开")
	}
}

// ---------------------------------------------------------------------------
// CommandKind
// ---------------------------------------------------------------------------

func TestCommandKindInterruptible(t *testing.T) {
	// 这个判断直接决定前端 Ctrl+C 按钮是否可用（架构 §6.5）。
	if !KindTUI.Interruptible() {
		t.Error("TUI 应当可以接收 Ctrl+C")
	}
	if KindShell.Interruptible() {
		t.Error("shell 在 Windows 上无法接收 Ctrl+C，不该被判为可中断")
	}
	if CommandKind("bogus").Valid() {
		t.Error("未知 kind 不该被判为合法")
	}
}
