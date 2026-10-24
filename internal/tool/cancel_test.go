package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// fakeCanceller 记录撤销请求。
type fakeCanceller struct {
	ids      []string
	reason   string
	cancel   int
	revoked  int
	rejected []string
}

func (f *fakeCanceller) CancelAssignments(_ context.Context, ids []string, reason string) (int, int, []string, error) {
	f.ids = append(f.ids, ids...)
	f.reason = reason
	return f.cancel, f.revoked, f.rejected, nil
}

func runCancel(t *testing.T, c Canceller, args string) (CancelAssignmentsOutput, error) {
	t.Helper()
	tl, err := NewCancelAssignmentsTool(c)
	if err != nil {
		t.Fatalf("build tool: %v", err)
	}
	inv, ok := tl.(tool.InvokableTool)
	if !ok {
		t.Fatalf("tool is not invokable: %T", tl)
	}
	out, err := inv.InvokableRun(context.Background(), args)
	if err != nil {
		return CancelAssignmentsOutput{}, err
	}
	var res CancelAssignmentsOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	return res, nil
}

// TestCancelAssignmentsTool 验证 ID 与原因被原样传给 Runtime。
func TestCancelAssignmentsTool(t *testing.T) {
	c := &fakeCanceller{cancel: 2, revoked: 1}
	res, err := runCancel(t, c, `{"assignment_ids":["a1","a2"],"reason":"目标重复"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Cancelled != 2 || res.RevokedResults != 1 {
		t.Fatalf("unexpected output: %+v", res)
	}
	if len(c.ids) != 2 || c.ids[0] != "a1" || c.ids[1] != "a2" {
		t.Fatalf("ids = %v", c.ids)
	}
	if c.reason != "目标重复" {
		t.Fatalf("reason = %q", c.reason)
	}
}

// TestCancelAssignmentsToolDedup 验证重复 ID 只提交一次、空白被忽略。
func TestCancelAssignmentsToolDedup(t *testing.T) {
	c := &fakeCanceller{cancel: 1}
	if _, err := runCancel(t, c, `{"assignment_ids":["a1"," a1 ",""],"reason":"x"}`); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(c.ids) != 1 || c.ids[0] != "a1" {
		t.Fatalf("ids = %v", c.ids)
	}
}

// TestCancelAssignmentsToolRequiresIDs 验证空列表直接报错。
func TestCancelAssignmentsToolRequiresIDs(t *testing.T) {
	c := &fakeCanceller{}
	if _, err := runCancel(t, c, `{"assignment_ids":[]}`); err == nil {
		t.Fatal("expected error when assignment_ids is empty")
	}
	if _, err := runCancel(t, c, `{"assignment_ids":["  "],"reason":"x"}`); err == nil {
		t.Fatal("expected error when all ids are blank")
	}
}

// TestCancelAssignmentsToolCaps 验证单次撤销上限。
func TestCancelAssignmentsToolCaps(t *testing.T) {
	c := &fakeCanceller{}
	ids := make([]string, 0, maxAssignmentIDsPerCall+5)
	for i := 0; i < maxAssignmentIDsPerCall+5; i++ {
		ids = append(ids, "a")
		_ = i
	}
	// 构造唯一 ID 以避免去重
	uniq := make([]string, 0, len(ids))
	for i := range ids {
		uniq = append(uniq, "a"+string(rune('0'+i%10))+string(rune('a'+i/10)))
	}
	payload, _ := json.Marshal(map[string]any{"assignment_ids": uniq, "reason": "批量"})
	res, err := runCancel(t, c, string(payload))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(c.ids) > maxAssignmentIDsPerCall {
		t.Fatalf("ids beyond cap were submitted: %d", len(c.ids))
	}
	if len(res.Rejected) == 0 {
		t.Fatal("cap overflow should be reported as rejected")
	}
}
