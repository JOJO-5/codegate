package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jojo/codegate/internal/protocol"
)

// 设备身份文件。
const (
	// keyFileName 是私钥文件名。
	//
	// ★ 它必须出现在 .gitignore 里。这是整个 Agent 唯一的长期秘密 ——
	// 拿到它就能冒充这台设备连上 Server。
	keyFileName = "device.key"

	// keyFileMode 是私钥的文件权限。
	//
	// ⚠️ Windows 上 os.Chmod 只影响只读位，**不提供真正的访问控制**
	// （NTFS ACL 由父目录继承决定）。所以这里 0600 主要是给 Unix 用的；
	// Windows 侧靠 `codegate-agent doctor` 提示"文件权限保护有限"，
	// 加固方案是 Phase 9 的 DPAPI（§10.2）。**不要以为设了 0600 就安全了。**
	keyFileMode = 0o600
)

// Identity 是这台设备的长期身份。
//
// 设计（§10.2 / §10.4）：
//   - 私钥只存在于本机，**从不上传**，Server 只保存公钥
//   - 鉴权用挑战-应答：Server 发 nonce，Agent 签名，nonce 一次性 + 30s TTL
//   - 因此不存在"长期 bearer token 被偷了就能永久冒充"的问题
type Identity struct {
	// DeviceID 是公钥的确定性派生值，不是随机 UUID ——
	// 这样同一份密钥永远得到同一个 ID，重装 Agent 不会变成"新设备"。
	DeviceID string

	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// LoadOrCreate 从 stateDir 读取设备身份，不存在则生成一份。
//
// 密钥一旦生成就不再变动：换密钥等于换设备，Server 侧要重新配对。
func LoadOrCreate(stateDir string) (*Identity, error) {
	if stateDir == "" {
		return nil, errors.New("agent: stateDir 为空，无法定位设备密钥")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: 创建状态目录 %s 失败: %w", stateDir, err)
	}

	path := filepath.Join(stateDir, keyFileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		id, perr := parsePrivateKey(data)
		if perr != nil {
			// 密钥文件损坏是**不可自动恢复**的：静默重新生成会让这台设备
			// 变成一台新设备，而用户看到的现象是"设备列表里多了一个"，
			// 完全对不上因果。所以这里必须报错并说清怎么处理。
			return nil, fmt.Errorf(
				"agent: 设备密钥 %s 无法解析（%w）。\n"+
					"  如果确认要重新配对，删除该文件后重启 Agent；\n"+
					"  注意这会让本机在 Server 侧变成一台**新设备**", path, perr)
		}
		return id, nil

	case errors.Is(err, os.ErrNotExist):
		return generateAndStore(path)

	default:
		return nil, fmt.Errorf("agent: 读取设备密钥 %s 失败: %w", path, err)
	}
}

// generateAndStore 生成新身份并原子落盘。
func generateAndStore(path string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("agent: 生成 Ed25519 密钥失败: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("agent: 序列化私钥失败: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if err := writeFileAtomic(path, block, keyFileMode); err != nil {
		return nil, fmt.Errorf("agent: 写入设备密钥 %s 失败: %w", path, err)
	}
	return newIdentity(pub, priv), nil
}

// parsePrivateKey 解析 PEM 编码的 PKCS8 私钥。
func parsePrivateKey(data []byte) (*Identity, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("不是合法的 PEM 文件")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS8 失败: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("密钥类型是 %T，不是 Ed25519", key)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("无法从私钥导出公钥")
	}
	return newIdentity(pub, priv), nil
}

func newIdentity(pub ed25519.PublicKey, priv ed25519.PrivateKey) *Identity {
	return &Identity{
		DeviceID: deriveDeviceID(pub),
		priv:     priv,
		pub:      pub,
	}
}

// deriveDeviceID 从公钥派生设备 ID。
//
// 取 SHA-256 的前 16 字节转 hex（32 字符）。不用完整 32 字节是因为
// 它要出现在日志、URL、UI 里，长度每多一倍可读性就降一档，
// 而 128 位对"防止碰撞"这个用途已经绰绰有余。
func deriveDeviceID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// PublicKey 返回 Ed25519 公钥。
//
// Server 侧验签需要它；测试里的假 Server 也用它。
func (id *Identity) PublicKey() ed25519.PublicKey { return id.pub }

// PublicKeyB64 返回 base64 编码的公钥（32 字节），用于 pair.begin。
func (id *Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(id.pub)
}

// Sign 用设备私钥签名。
func (id *Identity) Sign(payload []byte) []byte {
	return ed25519.Sign(id.priv, payload)
}

// Verify 校验签名。自检与测试用；生产路径上由 Server 校验。
func (id *Identity) Verify(payload, sig []byte) bool {
	return ed25519.Verify(id.pub, payload, sig)
}

// SigningPayload 构造挑战-应答的签名对象（§8.1）。
//
// 实现已上移到 protocol.SigningPayload —— 服务端也要用同一个字节格式，
// 各写一份的话，任何一端改了拼接顺序都会表现为「认证失败」，
// 而四个字完全看不出是编码问题。这里保留一层薄封装只是为了让
// agent 包内部的调用点不必多 import 一个包。
func SigningPayload(nonceB64, deviceID string, serverTime int64) ([]byte, error) {
	return protocol.SigningPayload(nonceB64, deviceID, serverTime)
}

// writeFileAtomic 先写临时文件再改名。
//
// 设备密钥写坏一半就再也读不回来，而这个文件没有备份 ——
// 所以哪怕多一次 rename 也要保证「要么是旧的完整内容，要么是新的完整内容」。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".device.key-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// 失败路径统一清理临时文件。成功路径 rename 之后它已经不存在了。
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // 已改名，不要再删
	return nil
}
