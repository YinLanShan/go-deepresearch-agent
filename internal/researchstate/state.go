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
// 一个 Task 对应唯一一份 ResearchState；它跨 RunTask 重建存活。
//
// 注意：Session 的 Title/Description（以及 Task 的 Title/Description）只作为
// 会话标识，不进入 Supervisor 提示词；Requirements 来自用户实际发送的消息。
//
// 三个易混淆字段的语义边界：
//   - Findings    ：模型的过程性输出（除工具调用外的全部 assistant 文本），每轮追加；
//   - Conclusion  ：该节点的最终结论，只有调用 finish_research 才写入，否则为空；
//   - Requirements：用户需求，按时间顺序累积（复数语义）。
type State struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`

	// Ordinal 是该 Task 在所属 Session 内的序号（从 1 开始），用于上下文分区标识。
	Ordinal int `json:"ordinal"`

	// Requirements 是用户在本研究任务中发送的全部消息，按时间顺序累积。
	//
	// 复数语义：一个 Task 内用户可能多次补充需求，全部进入上下文（编号列出）。
	Requirements []string `json:"requirements"`

	// Results 是已提交/待返回的研究产物（证据）。
	Results []Result `json:"results"`

	// Findings 是模型的过程性输出（除工具调用外的全部 assistant 文本）。
	Findings []Finding `json:"findings"`

	// Conclusion 是该研究节点的最终结论。
	//
	// 只有模型调用 finish_research 时才写入；未结束的研究该字段为空，
	// 这也是"该节点尚未完成"的天然标志。
	Conclusion string `json:"conclusion"`

	// RelatedNodes 是前序研究节点（已结束的 Task）的完整快照。
	RelatedNodes []RelatedNode `json:"related_nodes"`

	// InterruptedContext 是中间产物：本轮被新提交打断时暂存未完成的输出原文。
	//
	// 生命周期：
	//  1. 被打断时 Accumulate 追加原文；
	//  2. 只要该字段非空，BuildContext 就会渲染"上一轮被中断的输出"分区，
	//     使模型在重入时能看到中断前的实际上下文；
	//  3. Commit 时并入本轮 Finding，随后清空。
	InterruptedContext string `json:"interrupted_context"`

	// History 是本研究任务的对话历史（用户消息与 Supervisor 回复），
	// 仅用于前端切换会话时回放，不进入 Supervisor 提示词。
	History []Message `json:"history"`

	// CommitLog 是该节点的提交历史（只追加，不修改）。
	//
	// 提交与撤销都是"提交动作"，都会在此留下记录，用于审计与排查：
	// 每条记录包含操作类型、涉及的 Assignment / Result 以及原因。
	CommitLog []CommitRecord `json:"commit_log"`

	// Version 是版本号，每次提交动作（含撤销）自增。
	Version int64 `json:"version"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Result 是某一次具体执行产生的原始研究产物（证据）。
type Result struct {
	ID string `json:"id"`
	// AssignmentID 是产生该 Result 的 Assignment，用于撤销时精确定位。
	AssignmentID string       `json:"assignment_id,omitempty"`
	Turn         int          `json:"turn"`
	Content      string       `json:"content"`
	Status       ResultStatus `json:"status"`
	CreatedAt    time.Time    `json:"created_at"`
}

// CommitOperation 是提交历史中的操作类型。
type CommitOperation string

const (
	// OpCommitResult 表示提交一条研究产物。
	OpCommitResult CommitOperation = "commit_result"
	// OpCommitFinding 表示提交一条过程输出。
	OpCommitFinding CommitOperation = "commit_finding"
	// OpRevokeResult 表示撤销一条已提交的研究产物（不合法或被 Supervisor 撤回）。
	OpRevokeResult CommitOperation = "revoke_result"
	// OpSetConclusion 表示写入该节点的最终结论。
	OpSetConclusion CommitOperation = "set_conclusion"
)

// CommitRecord 是提交历史中的一条记录。
type CommitRecord struct {
	// Seq 是单调递增的序号（从 1 开始）。
	Seq int64 `json:"seq"`
	// Op 是操作类型。
	Op CommitOperation `json:"op"`
	// Turn 是所属轮次。
	Turn int `json:"turn,omitempty"`
	// AssignmentID 是相关 Assignment（提交/撤销 Result 时有值）。
	AssignmentID string `json:"assignment_id,omitempty"`
	// ResultID 是相关 Result（撤销时指向被撤销的那条）。
	ResultID string `json:"result_id,omitempty"`
	// Content 是操作涉及的内容摘要（撤销时保留原文，避免审计信息丢失）。
	Content string `json:"content,omitempty"`
	// Reason 是撤销原因等补充说明。
	Reason string `json:"reason,omitempty"`
	// Version 是本次操作完成后的版本号。
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// Finding 是模型的过程性输出（除工具调用外的全部 assistant 文本）。
type Finding struct {
	Turn      int       `json:"turn"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// RelatedNode 是前序研究节点（某个已结束的 Task）的完整快照。
//
// 保存完整快照而不是摘要，是为了让上下文构建能把历史节点完整展开，
// 避免后续节点只看到一句结论、丢失证据与过程输出。
type RelatedNode struct {
	TaskID       string    `json:"task_id"`
	Ordinal      int       `json:"ordinal"`
	Requirements []string  `json:"requirements"`
	Results      []Result  `json:"results"`
	Findings     []Finding `json:"findings"`
	// Conclusion 是该节点的最终结论（仅当该节点调用过 finish_research 才有值）。
	Conclusion string `json:"conclusion"`
	// InterruptedContext 是该节点残留的、未提交的被打断原文。
	//
	// 正常结束的节点该字段为空（Commit 已把原文并入 Finding 并清空）；
	// 但中止（stopped）的节点不会 Commit，此处会保留那一段未完成的输出，
	// 使后续节点不至于完全丢失。
	InterruptedContext string `json:"interrupted_context,omitempty"`
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

// ResultInput 是一条待提交的研究产物。
type ResultInput struct {
	// AssignmentID 是产生该产物的 Assignment（撤销时按它定位）。
	AssignmentID string
	// Content 是研究产物正文。
	Content string
}

// CommitInput 描述一次 ResearchState 提交。
//
// Commit 追加若干 Result 与至多一条 Finding，并把待合并的中断原文清空
// （其内容已并入本次 Finding）。每条 Result 独立保存以便后续按 Assignment 撤销。
type CommitInput struct {
	// Results 是本轮要提交的研究产物（可以为空）。
	Results []ResultInput
	// FindingContent 是模型本轮的过程性输出（已包含合并进来的中断原文）。
	FindingContent string
}

// New 创建一个研究任务的初始研究状态：不含任何需求与研究产物。
func New(id, taskID, sessionID string, ordinal int) *State {
	now := time.Now()
	return &State{
		ID:           id,
		TaskID:       taskID,
		SessionID:    sessionID,
		Ordinal:      ordinal,
		Requirements: []string{},
		Results:      []Result{},
		Findings:     []Finding{},
		CommitLog:    []CommitRecord{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// NewWithRelated 创建一份研究状态，并携带前序研究节点的完整快照。
func NewWithRelated(id, taskID, sessionID string, ordinal int, related []RelatedNode) *State {
	st := New(id, taskID, sessionID, ordinal)
	st.RelatedNodes = append([]RelatedNode(nil), related...)
	return st
}

// Summary 返回该研究状态的结论摘要（仅用于展示/兼容，不用于上下文构建）。
//
// 优先使用 Conclusion；未结束时退回过程 Finding 的摘要。
func (s *State) Summary() string {
	if strings.TrimSpace(s.Conclusion) != "" {
		return s.Conclusion
	}
	return s.findingsSummary()
}

// Snapshot 把当前节点转换为可供后续节点引用的完整快照。
func (s *State) Snapshot() RelatedNode {
	return RelatedNode{
		TaskID:             s.TaskID,
		Ordinal:            s.Ordinal,
		Requirements:       append([]string(nil), s.Requirements...),
		Results:            append([]Result(nil), s.Results...),
		Findings:           append([]Finding(nil), s.Findings...),
		Conclusion:         s.Conclusion,
		InterruptedContext: s.InterruptedContext,
	}
}

// pendingResultMark 是"未返回结果的 Result"在上下文模板中的固定提示词。
const pendingResultMark = "（该 Result 尚未返回结果）"

// BuildContext 构建 Supervisor 的输入上下文。
//
// 结构分区（对应 docs/Architecture.md 7.3.6）：前序节点按序完整展开 → 当前节点 →
// 当前任务与决策要求。每个节点内部按 需求 / Result / 最终结论 / 过程 Finding 分区，
// 当前节点若存在被中断的原文，会额外渲染一个"上一轮被中断的输出"分区。
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
	b.WriteString("【用户需求（按时间顺序）】\n")
	b.WriteString(requirementsSummaryOf(s.Requirements))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究产物 Result（第 %d 个节点的证据）】\n", s.Ordinal)
	b.WriteString(s.resultsSummary())
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【最终结论 Conclusion（第 %d 个节点的结论）】\n", s.Ordinal)
	b.WriteString(orEmpty(s.Conclusion, "（尚未结束研究，暂无结论）"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【过程输出 Finding（第 %d 个节点的推理过程）】\n", s.Ordinal)
	b.WriteString(s.findingsSummary())
	b.WriteString("\n\n")

	// 被中断的原文：只要该字段有内容就必须渲染。
	//
	// 它是"上一轮被打断、尚未提交"的输出片段；如果不渲染，模型在重入时
	// 就看不到自己中断前写到哪了，等于丢失了实际上下文。
	// 该字段在 Commit 时并入本轮 Finding 后清空，因此渲染是临时的。
	if strings.TrimSpace(s.InterruptedContext) != "" {
		fmt.Fprintf(&b, "【上一轮被中断的输出（第 %d 个节点，未完成）】\n", s.Ordinal)
		b.WriteString(s.InterruptedContext)
		b.WriteString("\n\n")
		b.WriteString("说明：以上是上一轮产出过程中被打断的片段，尚未提交为正式结论。\n")
		b.WriteString("请在此基础上续写或重写，避免重复已写过的部分。\n\n")
	}

	b.WriteString("# 当前任务与决策要求\n")
	b.WriteString("基于上述前序节点与当前节点，继续推进研究并输出本轮的工作内容。\n")
	b.WriteString("需要外部资料时调用工具（例如 web_search）；也可以调用 submit_assignments " +
		"把可独立完成的研究子任务派发到后台并行推进。\n")
	b.WriteString("派发子任务后无需等待，可以直接结束本轮；子任务结果提交后会唤醒你继续决策。\n")
	b.WriteString("当研究目标已达成、没有必须继续的工作时，调用 finish_research 给出整项研究的最终结论。\n")
	b.WriteString("未返回结果的 Result 不得视为已完成事实，不要虚构其结果。\n")
	b.WriteString("不要重复已经得出的结论。\n")

	return b.String()
}

// render 把前序节点完整展开为上下文段落。
func (n RelatedNode) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 第 %d 个研究节点\n\n", n.Ordinal)

	b.WriteString("【用户需求（按时间顺序）】\n")
	b.WriteString(requirementsSummaryOf(n.Requirements))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【研究产物 Result（第 %d 个节点的证据）】\n", n.Ordinal)
	b.WriteString(resultsSummaryOf(n.Results))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【最终结论 Conclusion（第 %d 个节点的结论）】\n", n.Ordinal)
	b.WriteString(orEmpty(n.Conclusion, "（该节点未宣布结束，暂无结论）"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "【过程输出 Finding（第 %d 个节点的推理过程）】\n", n.Ordinal)
	b.WriteString(findingsSummaryOf(n.Findings))
	b.WriteString("\n")

	// 中止节点残留的未提交原文（正常结束的节点该字段为空）。
	if strings.TrimSpace(n.InterruptedContext) != "" {
		fmt.Fprintf(&b, "\n【该节点中止时未完成的输出（第 %d 个节点，未提交）】\n", n.Ordinal)
		b.WriteString(n.InterruptedContext)
		b.WriteString("\n")
	}

	return b.String()
}

// requirementsSummaryOf 把用户需求按时间顺序编号列出。
func requirementsSummaryOf(reqs []string) string {
	if len(reqs) == 0 {
		return "（尚未提供，等待用户输入）"
	}
	var b strings.Builder
	for i, r := range reqs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, r)
	}
	return strings.TrimRight(b.String(), "\n")
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

// AddRequirement 追加一条用户需求（按时间顺序累积）。
// 空白内容被忽略；返回是否实际写入。
func (s *State) AddRequirement(requirement string) bool {
	requirement = strings.TrimSpace(requirement)
	if requirement == "" {
		return false
	}
	s.Requirements = append(s.Requirements, requirement)
	s.UpdatedAt = time.Now()
	return true
}

// AppendCommitRecord 追加一条提交历史记录（自动分配序号）。
//
// 调用方负责在此之前自增 Version，以便记录里带上本次操作完成后的版本号。
func (s *State) AppendCommitRecord(rec CommitRecord) {
	rec.Seq = int64(len(s.CommitLog)) + 1
	s.CommitLog = append(s.CommitLog, rec)
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
