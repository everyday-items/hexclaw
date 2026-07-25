package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestSolveAdapterGenerateParentTeachingGuideUsesExactProblemAndVerifiedSolution(t *testing.T) {
	var gotSubject, gotPrompt, gotGrade string
	adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
		_ context.Context,
		subject, prompt, grade string,
	) (string, error) {
		gotSubject, gotPrompt, gotGrade = subject, prompt, grade
		return `{
			"answer":"generator answer",
			"full_solution_steps":["generator copy is ignored by the usecase"],
			"grade_level_method":"先按整数乘法计算，再按小数位数点回小数点",
			"likely_mistakes":["把小数点点成两位"],
			"parent_teaching_sequence":["先让孩子算45×2","再问积的小数点放在哪里"],
			"follow_up_questions":["如果改成0.45×2，结果怎样变化？"],
			"checking_method":"用9÷2=4.5验算"
		}`, nil
	}))

	guide, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Subject: "数学", Grade: "五年级上", Problem: "4.5×2=",
		StudentAnswer: "8", KnowledgePoints: []string{"小数乘法"},
		WrongStep: "45×2 误算为 80", ErrorCause: "乘法事实错误",
		VerifiedSolution: "verified: 9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotSubject != "数学" || gotGrade != "五年级上" {
		t.Fatalf("subject/grade=%q/%q", gotSubject, gotGrade)
	}
	for _, exact := range []string{
		"4.5×2=", "verified: 9", "小数乘法", `"student_answer":"8"`,
		`"wrong_step":"45×2 误算为 80"`, `"error_cause":"乘法事实错误"`,
	} {
		if !strings.Contains(gotPrompt, exact) {
			t.Fatalf("prompt omitted exact per-question fact %q:\n%s", exact, gotPrompt)
		}
	}
	for _, field := range []string{
		"answer", "full_solution_steps", "grade_level_method", "likely_mistakes",
		"parent_teaching_sequence", "follow_up_questions", "checking_method",
	} {
		if !strings.Contains(gotPrompt, field) {
			t.Fatalf("prompt omitted approved field %q:\n%s", field, gotPrompt)
		}
	}
	if guide.GradeLevelMethod != "先按整数乘法计算，再按小数位数点回小数点" ||
		len(guide.FullSolutionSteps) != 1 ||
		len(guide.ParentTeachingSequence) != 2 ||
		len(guide.FollowUpQuestions) != 1 ||
		guide.CheckingMethod != "用9÷2=4.5验算" {
		t.Fatalf("structured guide parse failed: %#v", guide)
	}
}

func TestSolveAdapterGenerateParentTeachingGuideFailsHonestlyWithoutCapability(t *testing.T) {
	adapter := NewSolveAdapter(nil)
	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Problem: "1+1=", VerifiedSolution: "2",
	})
	if err == nil || !strings.Contains(err.Error(), "未注入") {
		t.Fatalf("missing guide capability error=%v", err)
	}
}

func TestSolveAdapterGenerateParentTeachingGuideRejectsMalformedJSON(t *testing.T) {
	adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
		context.Context, string, string, string,
	) (string, error) {
		return "not structured guide JSON", nil
	}))
	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Problem: "1+1=", VerifiedSolution: "2",
	})
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("malformed guide response error=%v", err)
	}
}

func TestSolveAdapterGenerateParentTeachingGuideRejectsUncontractedGenericFields(t *testing.T) {
	adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
		context.Context, string, string, string,
	) (string, error) {
		return `{
			"answer":"2",
			"full_solution_steps":["1+1=2"],
			"grade_level_method":"把两个加数相加",
			"likely_mistakes":["看错加号"],
			"parent_teaching_sequence":["先指出两个加数，再让孩子相加"],
			"follow_up_questions":["交换两个加数后结果怎样？"],
			"checking_method":"交换加数再算",
			"generic_tip":"认真一点"
		}`, nil
	}))
	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Problem: "1+1=", VerifiedSolution: "2",
	})
	if err == nil || !strings.Contains(err.Error(), "generic_tip") {
		t.Fatalf("uncontracted generic field error=%v", err)
	}
}

func TestSolveAdapterGenerateParentTeachingGuideRejectsStaleSemanticField(t *testing.T) {
	adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
		context.Context, string, string, string,
	) (string, error) {
		return `{
			"answer":"2",
			"full_solution_steps":["1+1=2"],
			"grade_level_method":"把两个加数相加",
			"likely_mistakes":["看错加号"],
			"parent_teaching_sequence":["先指出两个加数，再让孩子相加"],
			"follow_up_questions":["交换两个加数后结果怎样？"],
			"checking_method":"交换加数再算",
			"knowledge_point":"加法"
		}`, nil
	}))
	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Problem: "1+1=", VerifiedSolution: "2",
	})
	if err == nil || !strings.Contains(err.Error(), "knowledge_point") {
		t.Fatalf("stale semantic field error=%v", err)
	}
}
