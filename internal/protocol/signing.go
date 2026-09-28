package protocol

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// SigningPayload 构造 Ed25519 挑战-应答的签名对象（§8.1）。
//
// 对象是 `nonce || device_id || server_time` 的字节拼接：
//
//   - 带 nonce：防重放（nonce 本身一次性 + 30s TTL）
//   - 带 device_id：把签名绑死在这台设备上，防止把 A 设备的签名挪用到 B
//   - 带 server_time：防止攻击者用自己选的时钟构造出可长期复用的签名
//
// ★ 这个函数放在 protocol 包里，而不是各端各写一份。
//
// 拼接格式没有自描述结构（没有长度前缀、没有分隔符），因为双方都是
// 我们自己的代码 —— 代价是**任何一端的字节顺序或编码写错，签名就永远
// 验不过**，而症状只是「认证失败」四个字，完全看不出是编码问题。
// 一份实现放在共享包里，这个风险就不存在了。
//
// 将来若有第三方实现，应当升级成带长度前缀的规范编码，
// 并同时递增协议版本号。
func SigningPayload(nonceB64, deviceID string, serverTime int64) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, fmt.Errorf("%w: nonce 不是合法 base64: %v", ErrInvalidPayload, err)
	}

	buf := make([]byte, 0, len(nonce)+len(deviceID)+8)
	buf = append(buf, nonce...)
	buf = append(buf, deviceID...)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(serverTime))
	buf = append(buf, ts[:]...)
	return buf, nil
}
