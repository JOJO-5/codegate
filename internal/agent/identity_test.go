package agent

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("首次生成身份失败: %v", err)
	}
	if id1.DeviceID == "" {
		t.Fatal("DeviceID 为空")
	}

	// 密钥文件必须真的落盘 —— 否则每次重启都会变成新设备。
	path := filepath.Join(dir, keyFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件没有落盘: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("密钥文件是空的")
	}

	id2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if id2.DeviceID != id1.DeviceID {
		t.Errorf("二次加载得到了不同的 DeviceID: %s vs %s", id1.DeviceID, id2.DeviceID)
	}
}

// TestDeviceIDDerivesFromPublicKey 验证 DeviceID 是公钥的确定性派生，
// 而不是随机值 —— 否则把密钥复制到另一台机器会变成两个设备。
func TestDeviceIDDerivesFromPublicKey(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	id1, err := LoadOrCreate(src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(src, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, keyFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	id2, err := LoadOrCreate(dst)
	if err != nil {
		t.Fatal(err)
	}
	if id1.DeviceID != id2.DeviceID {
		t.Errorf("同一份密钥派生出了不同的 DeviceID: %s vs %s", id1.DeviceID, id2.DeviceID)
	}
	if id1.PublicKeyB64() != id2.PublicKeyB64() {
		t.Error("同一份密钥导出了不同的公钥")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	msg := []byte("nonce-bytes-device-id-server-time")
	sig := id.Sign(msg)
	if len(sig) != 64 {
		t.Errorf("Ed25519 签名应为 64 字节，实际 %d", len(sig))
	}
	if !id.Verify(msg, sig) {
		t.Error("自己签的名自己验不过")
	}
	if id.Verify([]byte("tampered"), sig) {
		t.Error("被篡改的消息居然验签通过了")
	}
}

// TestSigningPayloadDeterministic 是关键契约：
// Agent 与 Server 必须对同一组输入算出**逐字节相同**的待签名数据。
// 只要有一方多一个分隔符或少一个字节，鉴权就永远失败，
// 而错误现象是"签名无效"，完全指不到真正的原因。
func TestSigningPayloadDeterministic(t *testing.T) {
	nonce := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x07}, 32))
	const (
		deviceID   = "abcdef0123456789abcdef0123456789"
		serverTime = int64(1759000000000)
	)

	a, err := SigningPayload(nonce, deviceID, serverTime)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SigningPayload(nonce, deviceID, serverTime)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("同样的输入算出了不同的待签名数据")
	}

	// nonce(32) + deviceID(32) + serverTime(8)
	if want := 32 + len(deviceID) + 8; len(a) != want {
		t.Errorf("待签名数据长度 = %d，期望 %d", len(a), want)
	}

	// 三个组成部分任意一个变化，结果都必须不同。
	if c, _ := SigningPayload(nonce, "other-device", serverTime); bytes.Equal(a, c) {
		t.Error("deviceID 变化后待签名数据没变")
	}
	if c, _ := SigningPayload(nonce, deviceID, serverTime+1); bytes.Equal(a, c) {
		t.Error("serverTime 变化后待签名数据没变")
	}
	otherNonce := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x08}, 32))
	if c, _ := SigningPayload(otherNonce, deviceID, serverTime); bytes.Equal(a, c) {
		t.Error("nonce 变化后待签名数据没变")
	}
}

func TestSigningPayloadRejectsBadNonce(t *testing.T) {
	if _, err := SigningPayload("!!!not-base64!!!", "dev", 0); err == nil {
		t.Error("非 base64 的 nonce 被接受了")
	}
}

// TestLoadOrCreateRejectsCorruptKey 验证「密钥损坏」不会被静默吞掉。
//
// 静默重新生成会让这台设备在 Server 侧变成一台新设备，
// 而用户看到的现象是"设备列表里多了一个"，完全对不上因果。
func TestLoadOrCreateRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("损坏的密钥文件被静默接受了")
	}
	// 错误信息必须给出可执行的下一步，否则用户只能删了重来（然后更困惑）。
	if !strings.Contains(err.Error(), "删除") {
		t.Errorf("错误信息没有说明如何处理: %v", err)
	}
}

func TestLoadOrCreateRequiresStateDir(t *testing.T) {
	if _, err := LoadOrCreate(""); err == nil {
		t.Error("空的 stateDir 被接受了")
	}
}

func TestLoadOrCreateCreatesStateDir(t *testing.T) {
	// stateDir 不存在时应当自动创建（首次运行的常见情形）。
	nested := filepath.Join(t.TempDir(), "a", "b", "c")
	if _, err := LoadOrCreate(nested); err != nil {
		t.Fatalf("未能自动创建状态目录: %v", err)
	}
	if fi, err := os.Stat(nested); err != nil || !fi.IsDir() {
		t.Errorf("状态目录没有被创建: err=%v", err)
	}
}
