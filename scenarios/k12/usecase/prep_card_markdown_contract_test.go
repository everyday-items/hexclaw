package usecase

import (
	"context"
	"strings"
	"testing"
)

type groundedMarkdownPrepReview struct {
	prepReviewSolver
	evidence string
}

func (g *groundedMarkdownPrepReview) GenerateGroundedPrepReview(
	_ context.Context,
	_, _, _ string,
	evidence string,
) (string, error) {
	g.evidence = evidence
	return "**核心概念**：小数乘法先按整数乘法计算。\n\n公式：$2.8 \\times 0.65 = 1.82$", nil
}

func TestBuildPrepCard_TextbookEvidenceBecomesTeachingMarkdown(t *testing.T) {
	gen := &groundedMarkdownPrepReview{}
	d, _ := newPipeline(t, gen, fakeGrader{}, nil)
	d.Grounding = fakeGrounding{found: true}
	d.PrepReview = gen

	card, err := d.BuildPrepCard(context.Background(), "mingming", "五年级上", []string{"小数乘法"})
	if err != nil {
		t.Fatal(err)
	}
	got := card.Sections[0].Content
	if gen.evidence == "" {
		t.Fatal("教材命中后必须把证据交给教学 Markdown 生成器，而不是直接展示检索原文")
	}
	if strings.Contains(got, "以下是从个人知识库") || strings.Contains(got, "相关度:") || strings.Contains(got, "--- 参考") {
		t.Fatalf("展示内容不得泄漏检索协议文本: %q", got)
	}
	if !strings.Contains(got, "**核心概念**") || !strings.Contains(got, `$2.8 \times 0.65 = 1.82$`) {
		t.Fatalf("教材证据应整理为可由 Markdown + KaTeX 渲染的教学内容: %q", got)
	}
	if card.Sections[0].SourceLabel != SrcTextbook {
		t.Fatalf("教材证据生成的教学 Markdown 仍应保留教材来源标签, got %q", card.Sections[0].SourceLabel)
	}
}
