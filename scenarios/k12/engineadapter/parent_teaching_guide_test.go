package engineadapter

import (
	"context"
	"fmt"
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

func TestSolveAdapterGenerateParentTeachingGuideConsumesBundledPedagogyAndMathSkills(t *testing.T) {
	var gotPrompt string
	adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
		_ context.Context,
		_, prompt, _ string,
	) (string, error) {
		gotPrompt = prompt
		return `{
			"answer":"9",
			"full_solution_steps":["4.5×2=9"],
			"grade_level_method":"先按整数乘法计算，再点小数点",
			"likely_mistakes":["小数点位置错误"],
			"parent_teaching_sequence":["先让孩子复述题意","再让孩子独立计算"],
			"follow_up_questions":["怎样回看结果是否合理？"],
			"checking_method":"用9÷2=4.5验算"
		}`, nil
	}))

	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Subject: "数学", Grade: "五年级上", Problem: "4.5×2=", VerifiedSolution: "4.5×2=9",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{
		"k12-pedagogy", "最近发展区", "家长是中间人",
		"math-tutor", "波利亚", "理解题目", "回顾检验",
	} {
		if !strings.Contains(gotPrompt, anchor) {
			t.Fatalf("家长讲题提示未消费教学 Skill 锚点 %q:\n%s", anchor, gotPrompt)
		}
	}
}

func TestSolveAdapterGenerateParentTeachingGuideConsumesBundledSubjectSkill(t *testing.T) {
	tests := []struct {
		subject string
		skill   string
		anchors []string
	}{
		{subject: "数学", skill: "math-tutor", anchors: []string{"波利亚四步", "理解题目", "回顾检验"}},
		{subject: "语文", skill: "chinese-tutor", anchors: []string{"共写不代写", "分点作答", "从原文找依据"}},
		{subject: "英语", skill: "english-tutor", anchors: []string{"用法先于规则", "只鼓励不打击", "不代写"}},
		{subject: "科学", skill: "science-tutor", anchors: []string{"结论来自证据", "5E 教学模式", "不编造实验结果"}},
		{subject: "信息科技", skill: "information-technology-tutor", anchors: []string{"PRIMM 教学法", "运行结果以系统沙箱回传为准", "不代写完整程序"}},
	}

	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			var gotPrompt string
			adapter := NewSolveAdapter(nil, WithParentTeachingGuideGen(func(
				_ context.Context,
				_, prompt, _ string,
			) (string, error) {
				gotPrompt = prompt
				return `{"answer":"2","full_solution_steps":["1+1=2"],"grade_level_method":"小学方法","likely_mistakes":["看错条件"],"parent_teaching_sequence":["先问再讲"],"follow_up_questions":["怎样检查？"],"checking_method":"换一种方法检查"}`, nil
			}))

			_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
				Subject: tt.subject, Grade: "五年级", Problem: "1+1=", VerifiedSolution: "1+1=2",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, anchor := range append([]string{tt.skill}, tt.anchors...) {
				if !strings.Contains(gotPrompt, anchor) {
					t.Fatalf("%s 家长讲题提示未消费学科 Skill 锚点 %q:\n%s", tt.subject, anchor, gotPrompt)
				}
			}
		})
	}
}

func TestSolveAdapterGenerateParentTeachingGuidePrefersInstalledSkillBodies(t *testing.T) {
	var gotPrompt string
	loader := func(name string) (string, error) {
		switch name {
		case "k12-pedagogy":
			return "---\nname: k12-pedagogy\nversion: 9.0.0\n---\n盘上教学法：家长是中间人；最近发展区；渐进提示三阶段。", nil
		case "math-tutor":
			return "---\nname: math-tutor\nversion: 9.0.0\n---\n盘上数学法：波利亚四步；理解题目；回顾检验。", nil
		default:
			return "", fmt.Errorf("unexpected skill %q", name)
		}
	}
	adapter := NewSolveAdapter(
		nil,
		WithParentTeachingSkillLoader(loader),
		WithParentTeachingGuideGen(func(_ context.Context, _, prompt, _ string) (string, error) {
			gotPrompt = prompt
			return `{"answer":"9","full_solution_steps":["4.5×2=9"],"grade_level_method":"小学方法","likely_mistakes":["点错小数点"],"parent_teaching_sequence":["先问再算"],"follow_up_questions":["怎样验算？"],"checking_method":"9÷2=4.5"}`, nil
		}),
	)
	_, err := adapter.GenerateParentTeachingGuide(context.Background(), usecase.ParentTeachingGuideRequest{
		Subject: "数学", Grade: "五年级上", Problem: "4.5×2=", VerifiedSolution: "4.5×2=9",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"盘上教学法", "盘上数学法", "Skill: k12-pedagogy (installed)", "Skill: math-tutor (installed)"} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("提示未优先消费已安装 Skill %q:\n%s", want, gotPrompt)
		}
	}
	if strings.Contains(gotPrompt, "方法论根基") {
		t.Fatal("已安装 Skill 有效时不得再叠加内嵌正文")
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
