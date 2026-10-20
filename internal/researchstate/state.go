// Package researchstate 定义研究发现资产 ResearchState 及其上下文构建能力。
//
// ResearchState 是单个研究任务（Task）的核心知识状态，是模型研究的全部上下文。
// 其生命周期与 Task 绑定：一个 Session 下多个 Task 各自拥有一份 ResearchState。
//
// 数据关系（对应 docs/Architecture.md 5.8/5.10）：
//   - Result 是某一次执行产生的原始研究产物（证据），只增不减；
//   - Finding 是 Supervisor 整理后的研究结论（知识），由 Commit 追加；
//   - InterruptedContext 累积保存中断时正在产生但未完成的上下文原文，
//     独立于 Commit —— Commit 不得消费或清空该字段；
//   - RelatedNodes 是前序研究节点（已结束的 Task）的完整快照，
//     用于把历史节点完整展开进上下文。
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
// 注意：Session 的 Title/Description（以及 Task 的 Title/Description）只作为
// 会话标识，不进入 Supervisor 提示词；ResearchState 的 Requirement 来自用户
// 实际发送的第一条消息。
type State struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`

	// Ordinal 是该 Task 在所属 Session 内的序号（从 1 开始），用于上下文分区标识。
	Ordinal int `json:"ordinal"`

	// Requirement 是用户在本次研究任务中实际发送的第一条消息内容。
	Requirement string `json:"requirement"`

	// Results 是已提交/待返回的研究产物（证据）。
	Results []Result `json:"results"`

	// Findings 是 Supervisor 整理后的研究结论（知识）。
	Findings []Finding `json:"findings"`

	// RelatedNodes 是前序研究节点（已结束的 Task）的完整快照。
	RelatedNodes []RelatedNode `json:"related_nodes"`

	// InterruptedContext 累积保存中断时正在产生但未完成的上下文原文。
	// 该字段不被 Commit 消费，始终作为独立分区进入上下文。
	InterruptedContext string `json:"interrupted_context"`

	// History 是本研究任务的对话历史（用户消息与 Supervisor 回复），
	// 用于前端切换会话时回放。内存存储，随 ResearchState 生命周期存活。
	History []Message `json:"history"`

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
type Finding struct {
	Turn      int       `json:"turn"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// RelatedNode 是前序研究节点（某个已结束的 Task）的完整快照。
//
// 保存完整快照而不是摘要，是为了让上下文构建能把历史节点完整展开，
// 避免后续节点只看到一句结论、丢失证据与被中断的推理原文。
type RelatedNode struct {
	TaskID      string    `json:"task_id"`
	Ordinal     int       `json:"ordinal"`
	Requirement string    `json:"requirement"`
	Results     []Result  `json:"results"`
	Findings    []Finding `json:"findings"`
	// InterruptedContext 是该节点中断时累积的上下文原文。
	InterruptedContext string `json:"interrupted_context"`
}

// MessageRole 是对话历史中一条消息的角色。
type MessageRole string

const (
	// RoleUser 表示用户消息。
	RoleUser MessageRole = "user"
	// RoleAssistant 表示 Supervisor（模型）的回复。
	RoleAssistant MessageRole = "assistant"
)

// Message 是一条对话历史记录。
//
// 对话历史按 ResearchState（即 Task）存储，用于前端切换会话时回放历史。
type Message struct {
	Role      MessageRole `json:"role"`
	Content   string      `json:"content"`
	CreatedAt time.Time   `json:"created_at"`
}

// CommitInput 描述一次 ResearchState 提交。
//
// Commit 只追加 Result 与 Finding，不触碰 InterruptedContext。
type CommitInput struct {
	// ResultContent 是研究产物（证据）。
	ResultContent string
	// FindingContent 是研究结论（知识）。
	FindingContent string
}

// New 创建一个研究任务的初始研究状态：仅含研究需求，无任何研究产物。
func New(id, taskID, sessionID string, ordinal int, requirement string) *State {
	now := time.Now()
	return &State{
		ID:          id,
		TaskID:      taskID,
		SessionID:   sessionID,
		Ordinal:     ordinal,
		Requirement: requirement,
		Results:     []Result{},
		Findings:    []Finding{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// NewWithRelated 创建一份研究状态，并携带前序研究节点的完整快照。
func NewWithRelated(id, taskID, sessionID string, ordinal int, requirement string, related []RelatedNode) *State {
	st := New(id, taskID, sessionID, ordinal, requirement)
	st.RelatedNodes = append([]RelatedNode(nil), related...)
	return st
}

// Summary 返回该研究状态的结论摘要（仅用于展示/兼容，不再用于上下文构建）。
func (s *State) Summary() string {
	return s.findingsSummary()
}

// Snapshot 把当前节点转换为可供后续节点引用的完整快照。
func (s *State) Snapshot() RelatedNode {
	return RelatedNode{
		TaskID:             s.TaskID,
		Ordinal:            s.Ordinal,
		Requirement:        s.Requirement,
		Results:            append([]Result(nil), s.Results...),
		Findings:           append([]Finding(nil), s.Findings...),
		InterruptedContext: s.InterruptedContext,
	}
}

// pendingResultMark 是"未返回结果的 Result"在上下文模板中的固定提示词。
const pendingResultMark = "（该 Result 尚未返回结果）"

// BuildContext 构建 Supervisor 的输入上下文。
//
// 结构分区（对应 docs/Architecture.md 7.3.6）：前序节点按序完整展开 → 当前节点 →
// 当前任务与决策要求。每个节点内部再细分 需求 / Result / Finding / 被中断上下文。
func (s *State) BuildContext() string {
	var b strings.Builder

	b.WriteString("你是一名深度研究 Supervisor，负责根据研究需求推进研究并产出研究结论。\n\n")
	b.WriteString("# 前序研究节点（已完成或已中止的历史，按时间顺序）\n")
	if len(s.RelatedNodes) == 0 {
		b.WriteString("（无前序节点）\n\n")
	} else {
		b.WriteString("\n")
		for _, node := range s.RelatedNodes {
			b.WriteString(node.render())
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "# 当前研究节点（第 %d 个）\n\n", s.Ordinal)
	b.WriteString("【研究需求】\n")
	b.WriteString(orEmpty(s.Requirement, "（尚未提供，等待用户输入）"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究产物 Result（第 %d 个节点的证据）】\n", s.Ordinal)
	b.WriteString(s.resultsSummary())
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究结论 Finding（第 %d 个节点的知识）】\n", s.Ordinal)
	b.WriteString(s.findingsSummary())
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【被中断的上下文原文（第 %d 个节点的中断记录）】\n", s.Ordinal)
	b.WriteString(orEmpty(s.InterruptedContext, "（暂无）"))
	b.WriteString("\n\n")

	b.WriteString("# 当前任务与决策要求\n")
	b.WriteString("基于上述前序节点与当前节点，继续推进研究，直接输出本轮的研究结论。\n")
	b.WriteString("你可以调用提供的工具（例如 web_search）获取外部资料；调用前请简要说明目的。\n")
	b.WriteString("未返回结果的 Result 不得视为已完成事实，不要虚构其结果。\n")
	b.WriteString("不要重复已经得出的结论。\n")

	return b.String()
}

// render 把前序节点完整展开为上下文段落。
func (n RelatedNode) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 第 %d 个研究节点\n\n", n.Ordinal)

	b.WriteString("【研究需求】\n")
	b.WriteString(orEmpty(n.Requirement, "（无）"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究产物 Result（第 %d 个节点的证据）】\n", n.Ordinal)
	b.WriteString(resultsSummaryOf(n.Results))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究结论 Finding（第 %d 个节点的知识）】\n", n.Ordinal)
	b.WriteString(findingsSummaryOf(n.Findings))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【被中断的上下文原文（第 %d 个节点的中断记录）】\n", n.Ordinal)
	b.WriteString(orEmpty(n.InterruptedContext, "（暂无）"))
	b.WriteString("\n")

	return b.String()
}

// resultsSummary 汇总当前节点的研究产物；未返回结果的 Result 用固定提示词标注。
func (s *State) resultsSummary() string { return resultsSummaryOf(s.Results) }

func resultsSummaryOf(results []Result) string {
	if len(results) == 0 {
		return "（暂无）"
	}
	var b strings.Builder
	for _, r := range results {
		if r.Status == ResultPending {
			fmt.Fprintf(&b, " - [第 %d 轮] %s %s\n", r.Turn, r.Content, pendingResultMark)
		} else {
			fmt.Fprintf(&b, " - [第 %d 轮] %s\n", r.Turn, r.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// findingsSummary 汇总当前节点的研究结论。
func (s *State) findingsSummary() string { return findingsSummaryOf(s.Findings) }

func findingsSummaryOf(findings []Finding) string {
	if len(findings) == 0 {
		return "（暂无）"
	}
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, " - [第 %d 轮] %s\n", f.Turn, f.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SetRequirementIfEmpty 在需求为空时写入（用于"首条消息即研究需求"）。
// 返回是否实际写入。
func (s *State) SetRequirementIfEmpty(requirement string) bool {
	if s.Requirement != "" || requirement == "" {
		return false
	}
	s.Requirement = requirement
	s.UpdatedAt = time.Now()
	return true
}

// AddMessage 追加一条对话历史。
func (s *State) AddMessage(role MessageRole, content string) {
	if content == "" {
		return
	}
	now := time.Now()
	s.History = append(s.History, Message{Role: role, Content: content, CreatedAt: now})
	s.UpdatedAt = now
}

// orEmpty 返回 s，为空时返回 fallback。
func orEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
