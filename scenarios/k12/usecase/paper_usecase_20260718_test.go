package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 2026-07-18 呈现物端到端契约（§4.13 + §3.8）：
//   ① 固化前预览（draft）与固化后正卷走同一渲染器——预览口径 = 固化产物口径（诚实预览）；
//   ② 预览无卷面号（固化才分配），响应必须显式标注 preview；
//   ③ 固化后题目卷/答案卷可按 kind 取回，题目卷无答案、答案卷有答案；
//   ④ 没有已验证题的草稿不出预览（与固化门同口径）。

func seedPaperBasket(t *testing.T, d interface {
	AddToBasket(ctx context.Context, agentName, sourceSession string, item k12.PracticeItem) (string, bool, error)
}, ctx context.Context, agent string) string {
	t.Helper()
	items := []k12.PracticeItem{
		{Subject: "英语", QuestionMarkdown: "默写：believe", ExpectedAnswerMarkdown: "believe",
			AddedVia: k12.PracticeAddedViaWeekly, VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "字符比对"},
		{Subject: "数学", QuestionMarkdown: "解方程：2x + 19 = 51", ExpectedAnswerMarkdown: "x = 16",
			AddedVia: k12.PracticeAddedViaWeekly, VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		{Subject: "科学", QuestionMarkdown: "闭合电路判断", AddedVia: k12.PracticeAddedViaWeekly,
			VerificationStatus: k12.PracticeItemPending},
	}
	id := ""
	for _, it := range items {
		var err error
		id, _, err = d.AddToBasket(ctx, agent, "s", it)
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// ①② 预览与正卷同口径：题序/题面一致；预览无卷面号、固化后有。
func TestPaperPreviewMatchesFinalized(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")

	prev, err := d.RenderPracticePaper(ctx, "xiaoming", id, k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Preview {
		t.Fatal("draft 出卷应标注 preview=true")
	}
	if prev.PaperNo != "" || strings.Contains(prev.Markdown, "P-") {
		t.Fatalf("预览不得出现卷面号（固化才分配），got %q", prev.PaperNo)
	}
	// 预览题序 = 固化题序（数学1 → 英语2；阻断科学题不出现）。
	for _, want := range []string{"1. 解方程：2x + 19 = 51", "2. 默写：believe"} {
		if !strings.Contains(prev.Markdown, want) {
			t.Fatalf("预览应含 %q（与固化同口径），got:\n%s", want, prev.Markdown)
		}
	}
	if strings.Contains(prev.Markdown, "闭合电路") {
		t.Fatal("阻断题不得出现在预览")
	}

	if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", ""); err != nil {
		t.Fatal(err)
	}
	fin, err := d.RenderPracticePaper(ctx, "xiaoming", id, k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if fin.Preview || fin.PaperNo == "" {
		t.Fatalf("固化后应为正卷（preview=false 且有卷面号），got preview=%v paperNo=%q", fin.Preview, fin.PaperNo)
	}
	for _, want := range []string{"1. 解方程：2x + 19 = 51", "2. 默写：believe"} {
		if !strings.Contains(fin.Markdown, want) {
			t.Fatalf("正卷题序应与预览一致，缺 %q:\n%s", want, fin.Markdown)
		}
	}
}

// ③ kind 分卷：题目卷无答案；答案卷有答案。
func TestPaperKindSeparation(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")
	if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", ""); err != nil {
		t.Fatal(err)
	}
	q, err := d.RenderPracticePaper(ctx, "xiaoming", id, k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.Markdown, "x = 16") {
		t.Fatal("题目卷不得含答案")
	}
	a, err := d.RenderPracticePaper(ctx, "xiaoming", id, k12.PaperKindAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Markdown, "x = 16") {
		t.Fatalf("答案卷应含答案，got:\n%s", a.Markdown)
	}
	// 非法 kind 拒绝。
	if _, err := d.RenderPracticePaper(ctx, "xiaoming", id, "poster"); err == nil {
		t.Fatal("非法 kind 应拒绝")
	}
}

// ④ 没有已验证题不出预览（与固化门同口径，家长向文案）。
func TestPaperPreviewRequiresVerified(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id, _, err := d.AddToBasket(ctx, "xiaoming", "s", k12.PracticeItem{
		Subject: "科学", QuestionMarkdown: "阻断题", VerificationStatus: k12.PracticeItemPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RenderPracticePaper(ctx, "xiaoming", id, k12.PaperKindQuestion); err == nil {
		t.Fatal("没有已验证题目时应拒绝出预览")
	}
}
