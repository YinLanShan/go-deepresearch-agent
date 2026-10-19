// Package researchstate 定义研究发现资产 ResearchState 及其上下文构建能力。
//
// ResearchState 是当前研究运行的核心知识状态，是模型研究的全部上下文。
// 其生命周期与 Session 绑定：除非发生 Session 级联删除，否则 RunTask/Task
// 的状态变更不影响 ResearchState。
//
// 数据关系（对应 docs/Architecture.md 5.8/5.10）：
//   - Result 是某一次执行产生的原始研究产物（证据）；
//   - Finding 是 Supervisor 整理后的研究结论（知识）；
//   - RelatedFindings 是支撑当前 Finding 的先前研究结论；
//   - InterruptedContext 累积保存中断时正在产生但未完成的上下文原文；
//   - Version 是版本号，每次 Commit 自增。
package researchstate

import (
	"fmt"
	"strings"
	"time"
)

// ResultStatus 是研究产物（Result）的状态。
type ResultStatus string

const (
	// ResultPending 表示该 Result 尚未返回结果（仍在执行）。
	ResultPending ResultStatus = "pending"
	// ResultCommitted 表示该 Result 已提交为已确认的研究产物。
	ResultCommitted ResultStatus = "committed"
)

// State 是某个研究任务（Task）的研究状态，是研究发现资产的权威来源。
//
// ResearchState 按 Task 归属（一个 Task 一份，随 RunTask 创建而创建）；
// 一个 Session 下多个 Task 各自拥有一份 ResearchState。
type State struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`

	// Requirement 是用户随 Task 提交的第一条研究需求内容。
	Requirement string `json:"requirement"`

	// Results 是已提交/待返回的研究产物（证据）。
	Results []Result `json:"results"`

	// Findings 是 Supervisor 整理后的研究结论（知识）。
	Findings []Finding `json:"findings"`

	// RelatedFindings 是支撑当前研究结论的先前研究结论。
	RelatedFindings []RelatedFinding `json:"related_findings"`

	// InterruptedContext 累积保存中断时正在产生但未完成的上下文原文。
	InterruptedContext string `json:"interrupted_context"`

	// Version 是版本号，每次 Commit 自增。
	Version int64 `json:"version"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Result 是某一次具体执行产生的原始研究产物（证据）。
type Result struct {
	ID        string       `json:"id"`
	Turn      int          `json:"turn"`
	Content   string       `json:"content"`
	Status    ResultStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
}

// Finding 是经过整理后的研究结论（知识）。
// 其 Content 通常由"被中断的上下文原文 + 模型回答"构成。
type Finding struct {
	Turn      int       `json:"turn"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// RelatedFinding 是支撑当前研究结论的先前研究结论（引用）。
type RelatedFinding struct {
	SessionID string `json:"session_id"`
	Summary   string `json:"summary"`
}

// CommitInput 描述一次 ResearchState 提交。
type CommitInput struct {
	// ResultContent 是研究产物（证据）。
	ResultContent string
	// FindingContent 是研究结论（知识），通常为"被中断上下文 + 模型回答"。
	FindingContent string
}

// New 创建一个研究任务的初始研究状态：仅含研究需求，无任何研究产物。
func New(id, taskID, sessionID, requirement string) *State {
	now := time.Now()
	return &State{
		ID:          id,
		TaskID:      taskID,
		SessionID:   sessionID,
		Requirement: requirement,
		Results:     []Result{},
		Findings:    []Finding{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// NewWithRelated 创建一份研究状态，并携带先前研究任务的结论作为 RelatedFinding。
// 当一个研究任务完成/中止后，用户继续对话会新建一个 Task 与一份新 ResearchState，
// 上一份研究状态的全部上下文以 RelatedFinding 的形式继承。
func NewWithRelated(id, taskID, sessionID, requirement string, related []RelatedFinding) *State {
	st := New(id, taskID, sessionID, requirement)
	st.RelatedFindings = append([]RelatedFinding(nil), related...)
	return st
}

// Summary 返回该研究状态的结论摘要，用于作为后续研究任务的 RelatedFinding 内容。
func (s *State) Summary() string {
	return s.findingsSummary()
}

// pendingResultMark 是"未返回结果的 Result"在上下文模板中的固定提示词。
const pendingResultMark = "（该 Result 尚未返回结果）"

// contextTemplate 是固定的、可拼接的 Supervisor 输入上下文模板，
// 对应 docs/Architecture.md 7.3.6 的上下文模板结构。
const contextTemplate = `你是一名深度研究 Supervisor，负责根据研究需求推进研究并产出研究结论。

【研究需求】
%s

【研究产物（Result / 证据）】
%s

【研究结论（Finding）】
%s

【被中断的上下文原文】（累积）
%s

【关联的先前研究结论】
%s

【当前任务与决策要求】
基于上述研究需求、研究产物与结论，继续推进研究，直接输出本轮的研究结论。
未返回结果的 Result 不得视为已完成事实，不要虚构其结果。
不要重复已经得出的结论。
`

// BuildContext 根据当前研究状态构建 Supervisor 的输入上下文（固定模板 + 占位符填充）。
func (s *State) BuildContext() string {
	return fmt.Sprintf(contextTemplate,
		s.Requirement,
		s.resultsSummary(),
		s.findingsSummary(),
		s.interruptedSummary(),
		s.relatedFindingsSummary(),
	)
}

// resultsSummary 将研究产物拼接为文本；未返回结果的 Result 用固定提示词标注。
func (s *State) resultsSummary() string {
	if len(s.Results) == 0 {
		return "（暂无）"
	}
	var b strings.Builder
	for _, r := range s.Results {
		if r.Status == ResultPending {
			fmt.Fprintf(&b, "[第 %d 轮] %s %s\n", r.Turn, r.Content, pendingResultMark)
		} else {
			fmt.Fprintf(&b, "[第 %d 轮] %s\n", r.Turn, r.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// findingsSummary 将研究结论拼接为文本。
func (s *State) findingsSummary() string {
	if len(s.Findings) == 0 {
		return "（暂无）"
	}
	var b strings.Builder
	for _, f := range s.Findings {
		fmt.Fprintf(&b, "[第 %d 轮] %s\n", f.Turn, f.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// interruptedSummary 返回累积的被中断上下文原文。
func (s *State) interruptedSummary() string {
	if s.InterruptedContext == "" {
		return "（暂无）"
	}
	return s.InterruptedContext
}

// relatedFindingsSummary 将关联的先前研究结论拼接为文本。
func (s *State) relatedFindingsSummary() string {
	if len(s.RelatedFindings) == 0 {
		return "（暂无）"
	}
	var b strings.Builder
	for _, rf := range s.RelatedFindings {
		fmt.Fprintf(&b, "[Session %s] %s\n", rf.SessionID, rf.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}
