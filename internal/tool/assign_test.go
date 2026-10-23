package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// fakeAssigner 记录模型提交的目标。
type fakeAssigner struct {
	goals    []string
	accepted int
	err      error
}

func (f *fakeAssigner) SubmitAssignments(_ context.Context, goals []string) (int, []string, error) {
	f.goals = append(f.goals, goals...)
	if f.err != nil {
		return 0, nil, f.err
	}
	f.accepted = len(goals)
	return len(goals), nil, nil
}

func runAssign(t *testing.T, a Assigner, args string) (SubmitAssignmentsOutput, error) {
	t.Helper()
	tl, err := NewSubmitAssignmentsTool(a)
	if err != nil {
		t.Fatalf("build tool: %v", err)
	}
	inv, ok := tl.(tool.InvokableTool)
	if !ok {
		t.Fatalf("tool is not invokable: %T", tl)
	}
	out, err := inv.InvokableRun(context.Background(), args)
	if err != nil {
		return SubmitAssignmentsOutput{}, err
	}
	var res SubmitAssignmentsOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal output %q: %v", out, err)
	}
	return res, nil
}

// TestSubmitAssignmentsTool 验证模型给出的目标被原样交给 Runtime。
func TestSubmitAssignmentsTool(t *testing.T) {
	a := &fakeAssigner{}
	res, err := runAssign(t, a, `{"assignments":[
		{"goal":"收集基础事实"},
		{"goal":"分析权衡与约束"}
	]}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Accepted != 2 || !res.Dispatched {
		t.Fatalf("unexpected output: %+v", res)
	}
	if len(a.goals) != 2 || a.goals[0] != "收集基础事实" || a.goals[1] != "分析权衡与约束" {
		t.Fatalf("goals = %v", a.goals)
	}
}

// TestSubmitAssignmentsToolRejectsEmpty 验证空目标被忽略。
func TestSubmitAssignmentsToolRejectsEmpty(t *testing.T) {
	a := &fakeAssigner{}
	res, err := runAssign(t, a, `{"assignments":[{"goal":"  "},{"goal":"有效目标"}]}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", res.Accepted)
	}
	if len(res.Rejected) == 0 {
		t.Fatal("empty goal should be reported as rejected")
	}
	if len(a.goals) != 1 || a.goals[0] != "有效目标" {
		t.Fatalf("goals = %v", a.goals)
	}
}

// TestSubmitAssignmentsToolAllEmpty 验证全部为空时返回错误。
func TestSubmitAssignmentsToolAllEmpty(t *testing.T) {
	a := &fakeAssigner{}
	tl, _ := NewSubmitAssignmentsTool(a)
	inv := tl.(tool.InvokableTool)
	if _, err := inv.InvokableRun(context.Background(), `{"assignments":[{"goal":""}]}`); err == nil {
		t.Fatal("expected error when no valid goal")
	}
}

// TestSubmitAssignmentsToolCaps 验证单次上限。
func TestSubmitAssignmentsToolCaps(t *testing.T) {
	a := &fakeAssigner{}
	var sb strings.Builder
	sb.WriteString(`{"assignments":[`)
	for i := 0; i < maxAssignmentsPerCall+3; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"goal":"目标"}`)
	}
	sb.WriteString(`]}`)

	res, err := runAssign(t, a, sb.String())
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Accepted != maxAssignmentsPerCall {
		t.Fatalf("accepted = %d, want %d", res.Accepted, maxAssignmentsPerCall)
	}
	if len(res.Rejected) != 3 {
		t.Fatalf("rejected = %v, want 3 entries", res.Rejected)
	}
}
