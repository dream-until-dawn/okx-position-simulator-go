// Package okxerr 提供与 OKX API v5 错误码对齐的错误类型。
//
// 模拟器的一项验收标准是「错误码与 OKX 对齐」——使用者拿到的错误应当与
// 真实调用 OKX 时拿到的一致，这样策略里的错误分支才能被真实地回测到。
//
// 本包中的错误码均取自对 OKX 接口的实际探测，未经证实的码不予收录。
package okxerr

import (
	"errors"
	"fmt"
)

// 通用参数类错误码。
const (
	CodeParamError    = "51000" // Parameter {param} error
	CodeParamEmpty    = "50014" // Parameter {param} can not be empty
	CodeParamRequired = "50015" // Either parameter {a} or {b} is required
)

// 产品与档位类错误码。
const (
	CodeInstNotExist = "51001" // Instrument ID doesn't exist
)

// Error 是携带 OKX 错误码的错误。
type Error struct {
	Code string // OKX 错误码，如 "51001"
	Msg  string // 错误描述
	err  error  // 被包装的底层错误，可为 nil
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("okx %s: %s: %v", e.Code, e.Msg, e.err)
	}
	return fmt.Sprintf("okx %s: %s", e.Code, e.Msg)
}

func (e *Error) Unwrap() error { return e.err }

// Is 使 errors.Is 按错误码判等，忽略描述文本的差异。
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// New 构造一个带错误码的错误。
func New(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Wrap 构造一个带错误码并包装底层错误的错误。
func Wrap(err error, code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...), err: err}
}

// HasCode 报告 err 链上是否存在指定错误码的 *Error。
func HasCode(err error, code string) bool {
	var e *Error
	for errors.As(err, &e) {
		if e.Code == code {
			return true
		}
		if e.err == nil {
			return false
		}
		err = e.err
	}
	return false
}

// CodeOf 返回 err 链上第一个 *Error 的错误码，不存在时返回空串。
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
