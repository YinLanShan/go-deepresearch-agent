// Package fault 提供项目统一的错误描述，保留 Go 原生 error 语义。
//
// 该设计对应 docs/Event-Error-Interrupt.md 第 9.3 与 12 节：
//
//	错误分层 = 错误在哪里发生/被包装
//	错误处理 = 谁有权决定怎么办
//	两者分离，错误本身不表达"应该怎么处理"。
//
// 约定：
//   - 所有字段都可 programmatic 判断（Code/Layer/Severity），不依赖字符串比对；
//   - Wrap 是幂等的：已包装过的错误不会被外层重新标记，保留最内层的判断；
//   - RetryableHint 只是参考信息，不代表系统必须自动重试。
package fault

import (
	"errors"
	"fmt"
	"time"
)

// Layer 标识错误发生的架构层级。
//
// 层级决定"谁来处理"，而不是"有多严重"。
type Layer string

const (
	// LayerInfrastructure 模型、MCP Tool、检索等外部依赖。
	LayerInfrastructure Layer = "infrastructure"
	// LayerWork SubAgent 的单次执行。
	LayerWork Layer = "work"
	// LayerScheduler Assignment 的调度与资源分配。
	LayerScheduler Layer = "scheduler"
	// LayerResult 结果接收、聚合与提交。
	LayerResult Layer = "result"
	// LayerRuntime 生命周期、恢复与全局控制。
	LayerRuntime Layer = "runtime"
	// LayerAPI HTTP 边界（参数校验与错误映射）。
	LayerAPI Layer = "api"
)

// Code 是稳定的错误标识，用于程序化判断而非字符串比对。
type Code string

const (
	// ─── infrastructure ────────────────────────────────────────────────
	CodeInvalidInput     Code = "invalid_input"
	CodeModelUnavailable Code = "model_unavailable"
	CodeRateLimited      Code = "rate_limited"
	CodeUnavailable      Code = "unavailable"
	CodeToolFailed       Code = "tool_failed"

	// ─── work ──────────────────────────────────────────────────────────
	CodeWorkTimeout       Code = "work_timeout"
	CodeWorkCanceled      Code = "work_canceled"
	CodeWorkFailed        Code = "work_failed"
	CodeWorkEmptyResult   Code = "work_empty_result"
	CodeWorkNeedsApproval Code = "work_needs_approval"

	// ─── scheduler ─────────────────────────────────────────────────────
	CodeAssignmentNotFound Code = "assignment_not_found"
	CodeSlotUnavailable    Code = "slot_unavailable"
	CodeWorkCreateFailed   Code = "work_create_failed"
	CodeInvalidTransition  Code = "invalid_transition"

	// ─── result ────────────────────────────────────────────────────────
	CodeResultRejected Code = "result_rejected"
	CodeCommitFailed   Code = "commit_failed"
	CodeStateConflict  Code = "state_conflict"
	CodeNotAccepting   Code = "not_accepting"

	// ─── runtime ───────────────────────────────────────────────────────
	CodeNotFound            Code = "not_found"
	CodeTaskNotFound        Code = "task_not_found"
	CodeRunTaskConflict     Code = "run_task_conflict"
	CodeRuntimeInitFailed   Code = "runtime_init_failed"
	CodeRuntimeShutdownFail Code = "runtime_shutdown_failed"
	CodeInternal            Code = "internal"

	// ─── api ───────────────────────────────────────────────────────────
	CodeBadRequest Code = "bad_request"
	CodeConflict   Code = "conflict"
)

// Severity 表示错误需要被感知的程度，决定是否上报前端以及上报级别。
//
// 它与 Layer 是两个维度：Work 层的错误可能很轻微（info），
// Runtime 层的错误也可能只是通知（warning）。
type Severity string

const (
	// SeverityInfo 辅助信息（如历史记录写入失败），仅记录与轻提示。
	SeverityInfo Severity = "info"
	// SeverityWarning 局部失败，研究可继续（如某个子任务失败）。
	SeverityWarning Severity = "warning"
	// SeverityError 需要用户注意或调整（如结果保存失败）。
	SeverityError Severity = "error"
	// SeverityFatal 研究无法继续（如提交门控失效、关键组件失效）。
	SeverityFatal Severity = "fatal"
)

// Error 是项目统一错误类型。
type Error struct {
	Code     Code
	Layer    Layer
	Severity Severity
	// Source 是产生错误的组件，如 "workpool.run"。
	Source string

	// 影响范围：用于把错误路由给正确的处理者。
	TaskID       string
	RunTaskID    string
	AssignmentID string
	WorkID       string

	Msg string
	// RetryableHint 仅作参考，不代表系统必须自动重试。
	RetryableHint bool
	// Err 是底层原始错误，不直接暴露给客户端。
	Err        error
	OccurredAt time.Time
}

func (e *Error) Error() string {
	prefix := string(e.Layer)
	if e.Severity != "" {
		prefix += "/" + string(e.Severity)
	}
	msg := fmt.Sprintf("[%s/%s] %s", prefix, e.Code, e.Msg)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap 返回底层错误，支持 errors.Is / errors.As。
func (e *Error) Unwrap() error { return e.Err }

// ClientMessage 返回可以安全暴露给客户端的说明（不含底层 Err）。
func (e *Error) ClientMessage() string {
	if e.Msg != "" {
		return e.Msg
	}
	return string(e.Code)
}

// SeverityOf 提取错误的严重级别；非 *Error 时返回 SeverityError。
//
// 未包装的错误按 error 处理：它已经发生了，但系统不知道该如何分类。
func SeverityOf(err error) Severity {
	var e *Error
	if errors.As(err, &e) && e.Severity != "" {
		return e.Severity
	}
	return SeverityError
}

// 构造与包装。

// Builder 用于链式构造错误，避免长参数列表。
type Builder struct{ e *Error }

// E 开始构造一个错误。
func E(code Code, layer Layer, msg string) *Builder {
	return &Builder{e: &Error{
		Code:       code,
		Layer:      layer,
		Severity:   SeverityError,
		Msg:        msg,
		OccurredAt: time.Now(),
	}}
}

// Severity 设置严重级别。
func (b *Builder) Severity(s Severity) *Builder { b.e.Severity = s; return b }

// Source 设置来源组件。
func (b *Builder) Source(s string) *Builder { b.e.Source = s; return b }

// IDs 设置影响范围。
func (b *Builder) IDs(taskID, runTaskID, assignmentID, workID string) *Builder {
	b.e.TaskID = taskID
	b.e.RunTaskID = runTaskID
	b.e.AssignmentID = assignmentID
	b.e.WorkID = workID
	return b
}

// Retryable 标记是否具备重试条件（仅提示）。
func (b *Builder) Retryable(v bool) *Builder { b.e.RetryableHint = v; return b }

// Wrap 设置底层错误并返回 *Error；err 为 nil 时返回 nil。
//
// 幂等：若 err 已经是 *Error，原样返回，不覆盖内层判断。
// 这保证跨层传递时，"最早、最了解情况"的那一层决定错误分类。
func (b *Builder) Wrap(err error) *Error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return existing
	}
	b.e.Err = err
	return b.e
}

// Build 返回不含底层错误的 *Error。
func (b *Builder) Build() *Error { return b.e }

// New 构造一个不含底层错误的业务错误。
//
// 保留这个简洁入口，便于已有调用点迁移。
func New(code Code, layer Layer, msg string) *Error {
	return E(code, layer, msg).Build()
}

// Wrap 包装底层错误；err 为 nil 时返回 nil。
//
// 幂等语义同 Builder.Wrap。
func Wrap(code Code, layer Layer, msg string, err error) *Error {
	return E(code, layer, msg).Wrap(err)
}

// CodeOf 提取错误的稳定标识；若不是 *Error 返回空字符串。
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// LayerOf 提取错误所属层级；若不是 *Error 返回空字符串。
func LayerOf(err error) Layer {
	var e *Error
	if errors.As(err, &e) {
		return e.Layer
	}
	return ""
}
