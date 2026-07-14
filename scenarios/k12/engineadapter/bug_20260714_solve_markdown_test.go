package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

type plainSolveMarkdownExec struct{}

func (plainSolveMarkdownExec) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{
		Content:  "计划：先理解题意\n\n第1步：计算 4.5×2。\n所以 4.5×2=9。\n\n答案：9\n\n> ✅ 已程序验算。",
		Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
	}, nil
}

// 空白作业走 /solve 后会直接展示并可发钉钉；模型的普通标签文本必须在 adapter
// 边界收口成真正的 GitHub Markdown，而不是只换了一个 MarkdownRenderer 组件。
func TestBUG20260714_BlankHomeworkSolutionBecomesGitHubMarkdown(t *testing.T) {
	a := NewSolveAdapter(plainSolveMarkdownExec{})
	res, err := a.SolveSubject(context.Background(), "数学", "4.5×2=?", "五年级下", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 解答", "## 答案", "**9**", "> ✅ 已程序验算。"} {
		if !strings.Contains(res.Solution, want) {
			t.Fatalf("solution missing %q:\n%s", want, res.Solution)
		}
	}
}
