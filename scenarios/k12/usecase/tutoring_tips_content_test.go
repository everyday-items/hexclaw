package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type tutoringTipsGroundingStub struct {
	found bool
}

func (s tutoringTipsGroundingStub) Ground(context.Context, string, string, string) (string, bool, error) {
	return "教材证据正文", s.found, nil
}

type tutoringTipsGeneratorStub struct {
	calls    int
	evidence string
	err      error
}

func (s *tutoringTipsGeneratorStub) GenerateTutoringTipsReview(context.Context, string, string, string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return "**核心概念**：先理解单位。", nil
}

func (s *tutoringTipsGeneratorStub) GenerateGroundedTutoringTipsReview(
	_ context.Context, _, _, _, evidence string,
) (string, error) {
	s.calls++
	s.evidence = evidence
	return "**核心概念**：小数乘法先按整数乘法计算。\n\n公式：$2.8 \\times 0.65 = 1.82$", nil
}

func TestTutoringTipsOverviewUsesTextbookEvidenceWithoutLeakingRetrievalProtocol(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{}
	d := Deps{Grounding: tutoringTipsGroundingStub{found: true}, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"小数乘法"})
	if generator.calls != 1 || generator.evidence != "教材证据正文" {
		t.Fatalf("grounded generator evidence=%q calls=%d", generator.evidence, generator.calls)
	}
	if section.SourceLabel != TutoringTipsSourceTextbook ||
		!strings.Contains(section.Content, `$2.8 \times 0.65 = 1.82$`) {
		t.Fatalf("grounded section=%+v", section)
	}
	if strings.Contains(section.Content, "相关度:") || strings.Contains(section.Content, "参考编号") {
		t.Fatalf("retrieval protocol leaked: %q", section.Content)
	}
}

func TestTutoringTipsOverviewHonestFallbackUsesApprovedSourceLegend(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{err: errors.New("provider unavailable")}
	d := Deps{Grounding: tutoringTipsGroundingStub{}, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"简易方程"})
	if generator.calls != 1 || section.SourceLabel != TutoringTipsSourceAI {
		t.Fatalf("fallback label=%q calls=%d", section.SourceLabel, generator.calls)
	}
	if !strings.Contains(section.Content, "本次未生成可靠讲解") {
		t.Fatalf("fallback was not honest: %q", section.Content)
	}
}

func TestTutoringTipsOverviewTextbookHitSkipsUngroundedGeneration(t *testing.T) {
	generator := &tutoringTipsGeneratorStub{}
	grounding := tutoringTipsGroundingStub{found: true}
	d := Deps{Grounding: grounding, TutoringTipsReview: generator}
	section := d.tutoringTipsOverview(context.Background(), "mingming", "五年级上", "数学", []string{"小数乘法"})
	if generator.calls != 1 {
		// The single call is the evidence-grounded transformation, never the
		// ungrounded generation method.
		t.Fatalf("grounded transformation calls=%d", generator.calls)
	}
	if section.SourceLabel != TutoringTipsSourceTextbook {
		t.Fatalf("source label=%q", section.SourceLabel)
	}
}
