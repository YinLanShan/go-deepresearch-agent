// Package fault 提供项目统一的错误包装，保留 Go 原生 error 语义。
//
// 该设计对应 docs/Event-Error-Interrupt.md 第 12 节的错误分层约定，
// 阶段 0 仅实现最小字段：稳定的 Code、所属 Layer、说明信息与底层 Cause。
// RetryableHint 等更丰富的字段留待阶段 4 的错误分类机制再补充。
package fault

import (
	"errors"
	"fmt"
)

// Layer 标识错误发生的架构层级。
type Layer string

const (
	LayerInfrastructure Layer = "infrastructure"
	LayerAPI            Layer = "api"
	LayerRuntime        Layer = "runtime"
)

// Code 是稳定的错误标识，用于程序化判断而非字符串比对。
type Code string

const (
	CodeInvalidInput     Code = "invalid_input"
	CodeNotFound         Code = "not_found"
	CodeModelUnavailable Code = "model_unavailable"
	CodeInternal         Code = "internal"
)

// Error 是项目统一错误类型。
type Error struct {
	Code  Code
	Layer Layer
	Msg   string
	Err   error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s/%s] %s: %v", e.Layer, e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("[%s/%s] %s", e.Layer, e.Code, e.Msg)
}

// Unwrap 返回底层错误，支持 errors.Is / errors.As。
func (e *Error) Unwrap() error { return e.Err }

// New 构造一个不含底层错误的业务错误。
func New(code Code, layer Layer, msg string) *Error {
	return &Error{Code: code, Layer: layer, Msg: msg}
}

// Wrap 包装底层错误。
func Wrap(code Code, layer Layer, msg string, err error) *Error {
	return &Error{Code: code, Layer: layer, Msg: msg, Err: err}
}

// CodeOf 提取错误的稳定标识；若不是 *Error 返回空字符串。
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
