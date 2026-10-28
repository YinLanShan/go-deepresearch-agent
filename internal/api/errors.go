package api

import (
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// errorBody 是统一的错误响应体。
//
// 只暴露面向用户的说明（Message）与稳定错误码（code），
// 内部细节（底层 Err）仅写日志，避免泄漏实现。
type errorBody struct {
	Error string `json:"error"`
	// Code 是稳定错误标识，客户端可据此做程序化判断。
	Code string `json:"code,omitempty"`
	// Severity 便于前端决定展示方式（error / fatal）。
	Severity string `json:"severity,omitempty"`
}

// writeError 把错误统一映射为 HTTP 响应。
//
// 映射规则：
//   - 未包装错误 → 500 + "internal error"（不泄漏细节）；
//   - fault.Error → 按其 Code 映射状态码，返回 ClientMessage。
func writeError(c *app.RequestContext, err error) {
	if err == nil {
		return
	}

	var fe *fault.Error
	if !errors.As(err, &fe) {
		// 尝试识别存储层哨兵错误，避免全部落成 500。
		switch {
		case errors.Is(err, storage.ErrNotFound), errors.Is(err, researchstate.ErrNotFound):
			c.JSON(http.StatusNotFound, errorBody{Error: "not found", Code: string(fault.CodeNotFound)})
			return
		case errors.Is(err, researchstate.ErrNotAccepting):
			c.JSON(http.StatusConflict, errorBody{
				Error: "research state is not accepting commits",
				Code:  string(fault.CodeNotAccepting),
			})
			return
		default:
			c.JSON(http.StatusInternalServerError, errorBody{
				Error: "internal error",
				Code:  string(fault.CodeInternal),
			})
			return
		}
	}

	c.JSON(statusForCode(fe.Code), errorBody{
		Error:    fe.ClientMessage(),
		Code:     string(fe.Code),
		Severity: string(fe.Severity),
	})
}

// writeErrorWithStatus 在已知期望状态码时使用（保留语义更精确的映射）。
//
// 与 writeError 的差别：调用方已明确这是"客户端可理解的失败"（如 400/409），
// 因此未包装错误也允许直接返回其说明；500 路径仍走 writeError 的脱敏策略。
func writeErrorWithStatus(c *app.RequestContext, status int, err error) {
	if err == nil {
		return
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		c.JSON(status, errorBody{
			Error:    fe.ClientMessage(),
			Code:     string(fe.Code),
			Severity: string(fe.Severity),
		})
		return
	}
	c.JSON(status, errorBody{
		Error:    err.Error(),
		Code:     string(codeForStatus(status)),
		Severity: string(fault.SeverityError),
	})
}

// codeForStatus 为已知状态码选择一个稳定错误码（未包装错误时使用）。
func codeForStatus(status int) fault.Code {
	switch status {
	case http.StatusBadRequest:
		return fault.CodeBadRequest
	case http.StatusNotFound:
		return fault.CodeNotFound
	case http.StatusConflict:
		return fault.CodeConflict
	case http.StatusServiceUnavailable:
		return fault.CodeUnavailable
	default:
		return fault.CodeInternal
	}
}

// statusForCode 把稳定错误码映射为 HTTP 状态码。
func statusForCode(code fault.Code) int {
	switch code {
	case fault.CodeInvalidInput, fault.CodeBadRequest:
		return http.StatusBadRequest
	case fault.CodeNotFound, fault.CodeTaskNotFound, fault.CodeAssignmentNotFound:
		return http.StatusNotFound
	case fault.CodeConflict, fault.CodeStateConflict, fault.CodeInvalidTransition,
		fault.CodeNotAccepting, fault.CodeRunTaskConflict:
		return http.StatusConflict
	case fault.CodeUnavailable, fault.CodeModelUnavailable, fault.CodeRateLimited:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
