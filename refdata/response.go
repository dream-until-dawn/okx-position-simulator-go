package refdata

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
)

// CodeOK 是 OKX 表示成功的 code 值。
const CodeOK = "0"

// Response 是 OKX API v5 的统一响应信封。
//
// 快照文件直接采用 OKX 的原始响应格式，因此这个信封同时服务于三处：
// 解析实时拉取的响应、解析内置快照、以及对拍时与真实响应做比对。
type Response[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}

// DecodeResponse 解析 OKX 响应信封，并在 code 非 "0" 时返回带错误码的错误。
func DecodeResponse[T any](b []byte) ([]T, error) {
	var r Response[T]
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析 OKX 响应失败: %w", err)
	}
	if r.Code != CodeOK {
		return nil, okxerr.New(r.Code, "%s", r.Msg)
	}
	return r.Data, nil
}
