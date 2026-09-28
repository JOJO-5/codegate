package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jojo/codegate/internal/protocol"
)

// maxBodyBytes 是 REST 请求体的大小上限。
//
// 64 KB 远超任何合法用例（登录、改名、配对码都是几十字节）。
// 设这个上限的理由不是省内存，而是**别让对端决定我们要分配多少** ——
// 没有 MaxBytesReader 时，一个声明了 Content-Length: 1e9 的请求
// 会让 json.Decoder 一直读到内存耗尽。
const maxBodyBytes = 64 << 10

// decodeJSON 解析请求体到 T。
//
// 返回 ok=false 表示已经写过响应了，调用方直接 return 即可。
// 这个约定让每个 handler 的头部都是同一句 `if !ok { return }`。
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T

	if r.Body == nil {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), "请求体为空")
		return out, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	// ★ 拒绝未知字段。
	//
	// 与协议层的取舍相反（那里刻意忽略未知字段以便演进），
	// 因为这两处的失败代价不同：协议消息里多个字段，旧端忽略它照样能工作；
	// 而 REST 请求体里字段名打错（`passwrod`）会被静默忽略，
	// 表现成「提交了但没生效」—— 这种 bug 排查起来极其费劲。
	dec.DisallowUnknownFields()

	if err := dec.Decode(&out); err != nil {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), bodyErrorText(err))
		return out, false
	}

	// 拒绝「一个 JSON 后面还跟着东西」。不检查的话，
	// `{"email":"a"}{"email":"b"}` 会被当成一次合法请求解析出 a，
	// 而后半段被静默丢弃 —— 中间若有代理做内容检查，两边看到的东西就不一样了。
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, string(protocol.CodeInvalidPayload), "请求体必须是单个 JSON 对象")
		return out, false
	}

	return out, true
}

// bodyErrorText 把解析错误转成一句对用户有用、又不泄露实现的话。
func bodyErrorText(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "请求体过大"
	}
	// 这里可以安全地把 json 包的错误文本带上：它描述的是**客户端发来的**
	// 内容（"unexpected end of JSON input"），不含服务端任何信息。
	return "请求体不是合法 JSON: " + err.Error()
}
