package resultagg

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// publishResult 发布一条 Work 结果。
func publishResult(t *testing.T, bus *event.Bus, assignmentID, workID, result string) {
	t.Helper()
	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkResultReadyEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: assignmentID, WorkID: workID, Result: result,
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// TestRevokeBeforeCommitRejectsResult 验证"等待提交"阶段被撤销后结果被门控拦下。
func TestRevokeBeforeCommitRejectsResult(t *testing.T) {
	a, states, _, bus := newTestAgg(t)
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// 先标记撤销，再投递结果：结果不应进入 ResearchState。
	if n := a.Revoke("r1", []string{"a1"}, "不再需要"); n != 0 {
		t.Fatalf("nothing committed yet, revoked=%d", n)
	}

	publishResult(t, bus, "a1", "w1", "不该被提交的证据")
	time.Sleep(150 * time.Millisecond)

	st, err := states.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Results) != 0 {
		t.Fatalf("revoked result must not be committed, got %+v", st.Results)
	}
	if st.Version != 0 {
		t.Fatalf("no commit should happen, version=%d", st.Version)
	}
}

// TestRevokeAfterCommitRemovesResult 验证"已提交"的合法结果也会被撤回，
// 且撤销在提交历史中留下记录。
func TestRevokeAfterCommitRemovesResult(t *testing.T) {
	a, states, sched, bus := newTestAgg(t)
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	publishResult(t, bus, "a1", "w1", "先合法提交的证据")

	// 等待提交完成
	if !waitFor(t, 3*time.Second, func() bool {
		committed, _ := sched.snapshot()
		return len(committed) == 1
	}) {
		t.Fatal("result should be committed first")
	}

	st, _ := states.Get(context.Background(), "t1")
	if len(st.Results) != 1 {
		t.Fatalf("precondition: want 1 result, got %+v", st.Results)
	}

	// 撤销已提交的结果
	n := a.Revoke("r1", []string{"a1"}, "模型决定撤回")
	if n != 1 {
		t.Fatalf("revoked committed results = %d, want 1", n)
	}

	st, _ = states.Get(context.Background(), "t1")
	if len(st.Results) != 0 {
		t.Fatalf("revoked result should be removed from state, got %+v", st.Results)
	}

	// 提交历史：提交 + 撤销
	var sawRevoke bool
	for _, rec := range st.CommitLog {
		if rec.Op == researchstate.OpRevokeResult {
			sawRevoke = true
			if rec.AssignmentID != "a1" || rec.Reason != "模型决定撤回" {
				t.Fatalf("revoke record = %+v", rec)
			}
			if !strings.Contains(rec.Content, "先合法提交的证据") {
				t.Fatalf("revoke record should keep content, got %+v", rec)
			}
		}
	}
	if !sawRevoke {
		t.Fatal("commit log should record the revoke")
	}

	// Scheduler 应收到撤销通知
	if revoked := sched.revokedSnapshot(); len(revoked) != 1 || revoked[0] != "a1" {
		t.Fatalf("scheduler revoked = %v", revoked)
	}
}

// TestRevokeBlocksDuplicateResult 验证槽位因提交删除后，重复投递仍被门控拦截。
func TestRevokeBlocksDuplicateResult(t *testing.T) {
	a, states, _, bus := newTestAgg(t)
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	publishResult(t, bus, "a1", "w1", "第一份证据")
	time.Sleep(150 * time.Millisecond)

	a.Revoke("r1", []string{"a1"}, "撤回")

	// 撤销之后再投递同一 Assignment 的新结果（槽位已不存在）
	publishResult(t, bus, "a1", "w2", "撤销后的新结果")
	time.Sleep(150 * time.Millisecond)

	st, _ := states.Get(context.Background(), "t1")
	for _, r := range st.Results {
		if strings.Contains(r.Content, "撤销后的新结果") {
			t.Fatalf("result after revoke must be blocked, got %+v", st.Results)
		}
	}
}

// TestRevokeOnlyAffectsTargetAssignment 验证撤销只影响指定 Assignment。
func TestRevokeOnlyAffectsTargetAssignment(t *testing.T) {
	a, states, sched, bus := newTestAgg(t)
	for _, id := range []string{"a1", "a2"} {
		if err := a.Reserve(slotAssignment(id, "r1")); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}

	publishResult(t, bus, "a1", "w1", "甲的证据")
	publishResult(t, bus, "a2", "w2", "乙的证据")
	if !waitFor(t, 3*time.Second, func() bool {
		committed, _ := sched.snapshot()
		return len(committed) == 2
	}) {
		t.Fatal("both results should be committed")
	}

	a.Revoke("r1", []string{"a1"}, "只撤销甲")

	st, _ := states.Get(context.Background(), "t1")
	if len(st.Results) != 1 {
		t.Fatalf("want 1 remaining result, got %+v", st.Results)
	}
	if st.Results[0].AssignmentID != "a2" {
		t.Fatalf("remaining result should belong to a2, got %+v", st.Results[0])
	}
}

// TestForgotRunTaskClearsRevokeIndex 验证 RunTask 删除时撤销索引被清理。
func TestForgotRunTaskClearsRevokeIndex(t *testing.T) {
	a, states, _, _ := newTestAgg(t)
	a.Revoke("r1", []string{"a1"}, "撤回")

	a.ForgotRunTask("r1")

	a.mu.Lock()
	_, exists := a.revokedRunTask["r1"]
	a.mu.Unlock()
	if exists {
		t.Fatal("revoke index should be cleared for deleted run task")
	}
	// 索引清理后，新预留的槽位可正常提交
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve after forget: %v", err)
	}
	if err := a.Reserve(slotAssignment("a9", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_ = states
}

// slotAssignment 构造一个用于预留槽位的 Assignment。
func slotAssignmentIn(id, runTaskID string) *domain.Assignment {
	return &domain.Assignment{
		ID: id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.AssignmentScheduled,
	}
}
