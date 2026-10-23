package config

import (
	"reflect"
	"testing"
)

// TestApprovalRequired 验证审批策略的解析，尤其是"-"/"none" 显式关闭。
func TestApprovalRequired(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "默认空值 = 全部审批", raw: "", want: []string{"*"}},
		{name: "破折号关闭", raw: "-", want: nil},
		{name: "none 关闭", raw: "none", want: nil},
		{name: "NONE 大小写不敏感", raw: "NONE", want: nil},
		{name: "带空格关闭", raw: "  none  ", want: nil},
		{name: "全部", raw: "*", want: []string{"*"}},
		{name: "单工具", raw: "web_search", want: []string{"web_search"}},
		{name: "多工具含空格", raw: "echo, add ", want: []string{"echo", "add"}},
		{name: "忽略空项", raw: "echo,,add", want: []string{"echo", "add"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ToolConfig{RequireApproval: c.raw}.ApprovalRequired()
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ApprovalRequired(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestApprovalPolicyNilMeansNoApproval 验证返回 nil 时 Policy 不会拦截任何工具。
func TestApprovalPolicyNilMeansNoApproval(t *testing.T) {
	tc := ToolConfig{RequireApproval: "-"}
	if got := tc.ApprovalRequired(); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}
