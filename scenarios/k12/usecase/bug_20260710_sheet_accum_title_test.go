package usecase

import (
	"context"
	"strings"
	"testing"
)

// BUG-20260710（错题卷空题干）：ReviewQueue 跨集合混排后含积累本纠错项（其 Fields.Question
// 为零值），MistakeSheetMarkdown 直接打印 it.Fields.Question → 渲染出空题干行。
// 应改用跨集合安全的 ReviewItem.Title()（错题项返回 Question，积累项返回 Accum.Content）。
func TestBug20260710_MistakeSheet_AccumItemHasTitle(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()

	// 积累本纠错项（英语错词），due=400 < now=1000 → 进 ReviewQueue；Fields.Question 零值。
	seedAccumDue(t, d, "英语", "错词", "believe", 400)
	// 一条正常错题，验证错题项题面输出语义不被改坏。
	seedMistake(t, d, "m1", "小数乘法", "计算失误", 500)

	md, err := d.MistakeSheetMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	// 积累项必须以 Title()（= Accum.Content）出现在卷面上。
	if !strings.Contains(md, "believe") {
		t.Errorf("错题卷应含积累项 Title() 文本 %q，got:\n%s", "believe", md)
	}
	// 错题项题面（Title()==Fields.Question）仍在。
	if !strings.Contains(md, "题-m1") {
		t.Errorf("错题卷应含错题项题面 %q，got:\n%s", "题-m1", md)
	}
	// 每个题号后必须有非空题面：不允许 "**N.** " 后面直接空到行尾（空题干行）。
	for _, line := range strings.Split(md, "\n") {
		idx := strings.Index(line, ".** ")
		if strings.HasPrefix(line, "**") && idx >= 0 {
			if strings.TrimSpace(line[idx+len(".** "):]) == "" {
				t.Errorf("出现空题干行 %q，全文:\n%s", line, md)
			}
		}
	}
}
