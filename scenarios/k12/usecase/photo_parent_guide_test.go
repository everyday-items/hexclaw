package usecase

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type blankWorksheetSolver struct {
	evidenceType EvidenceType
}

func (s blankWorksheetSolver) Solve(_ context.Context, problem, _, _ string) (SolveResult, error) {
	evidenceType := s.evidenceType
	if evidenceType == "" {
		evidenceType = EvidenceNumericExec
	}
	return SolveResult{
		Solution: blankWorksheetVerifiedSolution(problem),
		Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: evidenceType},
	}, nil
}

func blankWorksheetAnswer(problem string) string {
	switch problem {
	case "4.5×2=", "36÷4=":
		return "9"
	case "1+1=":
		return "2"
	case "2+2=":
		return "4"
	case "3+3=":
		return "6"
	default:
		return "2"
	}
}

func blankWorksheetVerifiedSolution(problem string) string {
	answer := blankWorksheetAnswer(problem)
	return fmt.Sprintf(
		"## 完整方法\n先根据题意计算 %s，并逐步核对中间结果。\n## 答案\n**%s**",
		problem, answer,
	)
}

func blankWorksheetVerifiedSteps(problem string) []string {
	return []string{"先根据题意计算 " + problem + "，并逐步核对中间结果。"}
}

type parentTeachingGuideSpy struct {
	mu    sync.Mutex
	calls []ParentTeachingGuideRequest
}

func (s *parentTeachingGuideSpy) GenerateParentTeachingGuide(
	_ context.Context,
	req ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	return ParentTeachingGuide{
		Answer:                 blankWorksheetAnswer(req.Problem),
		FullSolutionSteps:      []string{"untrusted generator rewrite"},
		GradeLevelMethod:       "本年级方法：" + req.Problem,
		LikelyMistakes:         []string{"易错：" + req.Problem},
		ParentTeachingSequence: []string{"先讲题意：" + req.Problem, "再让孩子自己计算"},
		FollowUpQuestions:      []string{"为什么这样计算：" + req.Problem + "？"},
		CheckingMethod:         "代回原题：" + req.Problem,
	}, nil
}

func (s *parentTeachingGuideSpy) snapshot() []ParentTeachingGuideRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ParentTeachingGuideRequest(nil), s.calls...)
}

func TestGradeHomeworkPhotoBlankWorksheetBuildsCompleteProblemSpecificParentGuides(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{evidenceType: EvidenceHeuristic}, fakeGrader{}, nil)
	generator := &parentTeachingGuideSpy{}
	d.ParentTeachingGuide = generator
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{
			Question: "4.5×2=", Subject: "数学", AnswerState: AnswerStateBlank,
			KnowledgePoints: []string{"小数乘法"},
		},
		{Question: "36÷4=", Subject: "数学", AnswerState: AnswerStateBlank},
	}}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("blank worksheet"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskIntent != PhotoTaskBlankWorksheet || got.ResultSurface != PhotoSurfaceParentTeachingGuide {
		t.Fatalf("blank worksheet semantics = %q/%q", got.TaskIntent, got.ResultSurface)
	}
	calls := generator.snapshot()
	if len(calls) != 2 {
		t.Fatalf("guide generator calls=%d, want one per question: %#v", len(calls), calls)
	}
	seenProblems := map[string]bool{}
	for _, call := range calls {
		seenProblems[call.Problem] = true
		if call.VerifiedSolution != blankWorksheetVerifiedSolution(call.Problem) {
			t.Fatalf("generator did not receive the verified solution for %q: %#v", call.Problem, call)
		}
	}
	for _, problem := range []string{"4.5×2=", "36÷4="} {
		if !seenProblems[problem] {
			t.Fatalf("question-specific guide request missing for %q: %#v", problem, calls)
		}
	}

	if len(got.Items) != 2 {
		t.Fatalf("items=%d, want 2", len(got.Items))
	}
	for i, item := range got.Items {
		if item.Status != PhotoBlankSolved || item.ResultKind != PhotoItemParentTeachingGuide || item.ParentGuide == nil {
			t.Fatalf("item %d lacks parent-guide result semantics: %#v", i, item)
		}
		guide := *item.ParentGuide
		wantAnswer := blankWorksheetAnswer(item.Recognized.Question)
		if guide.Answer != wantAnswer {
			t.Fatalf("item %d answer=%q, want solver-anchored final answer %q", i, guide.Answer, wantAnswer)
		}
		wantFullSolutionSteps := blankWorksheetVerifiedSteps(item.Recognized.Question)
		if !reflect.DeepEqual(guide.FullSolutionSteps, wantFullSolutionSteps) {
			t.Fatalf("item %d full_solution_steps=%#v, want deterministic verified steps %#v",
				i, guide.FullSolutionSteps, wantFullSolutionSteps)
		}
		if len(guide.FullSolutionSteps) == 1 && guide.Answer == guide.FullSolutionSteps[0] {
			t.Fatalf("item %d copied the entire solution into answer: %#v", i, guide)
		}
		if strings.TrimSpace(guide.GradeLevelMethod) == "" ||
			len(guide.LikelyMistakes) == 0 ||
			len(guide.ParentTeachingSequence) == 0 ||
			len(guide.FollowUpQuestions) == 0 ||
			strings.TrimSpace(guide.CheckingMethod) == "" {
			t.Fatalf("item %d guide is incomplete: %#v", i, guide)
		}
		if !strings.Contains(guide.LikelyMistakes[0], item.Recognized.Question) {
			t.Fatalf("item %d received repeated generic guidance: %#v", i, guide)
		}
	}

	for _, want := range []string{
		"家长辅导指南",
		"**答案：**", "**必要步骤：**", "**本年级方法：**", "**易错点：**",
		"**家长怎么讲：**", "**可以追问：**", "**怎么检查：**",
		"易错：4.5×2=", "易错：36÷4=",
	} {
		if !strings.Contains(got.Markdown, want) {
			t.Fatalf("parent-guide markdown missing %q:\n%s", want, got.Markdown)
		}
	}
	for _, stale := range []string{"**知识点：**", "**讲解步骤：**", "**批改标准：**", "**家长提问顺序：**", "**核对方法：**"} {
		if strings.Contains(got.Markdown, stale) {
			t.Fatalf("parent-guide markdown still exposes stale semantic item %q:\n%s", stale, got.Markdown)
		}
	}
}

type incompleteParentTeachingGuide struct{}

func (incompleteParentTeachingGuide) GenerateParentTeachingGuide(
	context.Context,
	ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	return ParentTeachingGuide{Answer: "model answer"}, nil
}

func TestGradeHomeworkPhotoBlankWorksheetRejectsIncompleteGenericGuide(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{evidenceType: EvidenceHeuristic}, fakeGrader{}, nil)
	d.ParentTeachingGuide = incompleteParentTeachingGuide{}
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question: "2+2=", Subject: "数学", AnswerState: AnswerStateBlank,
	}}}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("blank worksheet"),
	})
	if err == nil {
		t.Fatal("an incomplete generic guide must fail visibly")
	}
	if len(got.Items) != 1 || got.Items[0].Status != PhotoFailed || got.Items[0].ParentGuide != nil {
		t.Fatalf("incomplete guide was presented as complete: %#v, err=%v", got.Items, err)
	}
	if !strings.Contains(err.Error(), "parent teaching guide") {
		t.Fatalf("error must identify the failed contract, got %v", err)
	}
}

func TestGradeHomeworkPhotoCompletedHomeworkSolvesBlankItemsForParent(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{}, fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	generator := &parentTeachingGuideSpy{}
	d.ParentTeachingGuide = generator
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		{Question: "2+2=", Subject: "数学", AnswerState: AnswerStateBlank},
	}}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("completed homework"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskIntent != PhotoTaskCompletedHomework || got.ResultSurface != PhotoSurfaceAnnotatedHomework {
		t.Fatalf("completed-homework semantics = %q/%q", got.TaskIntent, got.ResultSurface)
	}
	if len(got.Items) != 2 ||
		got.Items[0].ResultKind != PhotoItemAssessment ||
		got.Items[1].Status != PhotoBlankSolved ||
		got.Items[1].ResultKind != PhotoItemParentTeachingGuide ||
		got.Items[1].ParentGuide == nil ||
		got.Items[1].Solve.Solution == "" || got.Items[1].Grade.RecordCreated {
		t.Fatalf("mixed completed-homework must solve blanks without grading them: %#v", got.Items)
	}
	if calls := generator.snapshot(); len(calls) != 0 {
		t.Fatalf("verified arithmetic must use the local parent guide without another provider call: %#v", calls)
	}
}

func TestSolveHomeworkProblemLegacyClientDoesNotRequireParentGuideGenerator(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{}, fakeGrader{}, nil)
	res, err := d.SolveHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上", Problem: "3+3=",
	})
	if err != nil || res.Solution != blankWorksheetVerifiedSolution("3+3=") {
		t.Fatalf("legacy solve client changed: result=%#v err=%v", res, err)
	}
}

func TestBlankWorksheetGuidePassesOnlyNonEmptyRecognizedKnowledgePointsAsGenerationFacts(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{evidenceType: EvidenceHeuristic}, fakeGrader{}, nil)
	generator := &parentTeachingGuideSpy{}
	d.ParentTeachingGuide = generator
	result, err := d.SolveBlankWorksheetProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上", Problem: "3+3=",
		KnowledgePoints: []string{" ", "整数加法", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls := generator.snapshot(); len(calls) != 1 ||
		len(calls[0].KnowledgePoints) != 1 || calls[0].KnowledgePoints[0] != "整数加法" {
		t.Fatalf("guide generation facts did not normalize recognized knowledge points: %#v", calls)
	}
	if result.Guide.GradeLevelMethod != "本年级方法：3+3=" {
		t.Fatalf("grade-level method was not retained: %#v", result.Guide)
	}
}

func TestParentTeachingGuideValidationErrorNamesMissingField(t *testing.T) {
	err := validateParentTeachingGuide(ParentTeachingGuide{
		Answer:                 "2",
		FullSolutionSteps:      []string{"把 1 和 1 相加得到 2"},
		GradeLevelMethod:       "把两个加数相加",
		LikelyMistakes:         []string{"写错符号"},
		ParentTeachingSequence: []string{"先让孩子指出两个加数"},
		FollowUpQuestions:      []string{"交换两个加数后结果怎样？"},
	})
	if err == nil || !strings.Contains(err.Error(), "checking_method") {
		t.Fatalf("validation error=%v, want missing checking_method", err)
	}
}

type intermediateResultAnswerGuide struct{}

func (intermediateResultAnswerGuide) GenerateParentTeachingGuide(
	context.Context,
	ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	return ParentTeachingGuide{
		Answer:                 "90",
		FullSolutionSteps:      []string{"generator-controlled"},
		GradeLevelMethod:       "先按整数乘法算，再点回一位小数",
		LikelyMistakes:         []string{"忘记点回小数点"},
		ParentTeachingSequence: []string{"先算 45×2，再让孩子点小数点"},
		FollowUpQuestions:      []string{"积应该有几位小数？"},
		CheckingMethod:         "用除法反向验算",
	}, nil
}

func TestBlankWorksheetGuideRejectsIntermediateResultAsFinalAnswer(t *testing.T) {
	d, _ := newPipeline(t, blankWorksheetSolver{evidenceType: EvidenceHeuristic}, fakeGrader{}, nil)
	d.ParentTeachingGuide = intermediateResultAnswerGuide{}

	result, err := d.SolveBlankWorksheetProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上", Problem: "4.5×2=",
	})
	if err == nil {
		t.Fatalf("intermediate result was accepted as final answer: %#v", result.Guide)
	}
	if !strings.Contains(err.Error(), "answer") || !strings.Contains(err.Error(), "verified solution") {
		t.Fatalf("unanchored answer error=%v, want explicit solver-anchor failure", err)
	}
}

func TestVerifiedFullSolutionStepsPreserveOrderedParagraphsAndExcludeAnswerSection(t *testing.T) {
	solution := "## 方法\n\n1. 先列式 45×2=90。\n2. 再点回一位小数。\n\n补充核对数量级。\n\n## 答案\n**9**"
	got := verifiedFullSolutionSteps(solution)
	want := []string{
		"先列式 45×2=90。",
		"再点回一位小数。",
		"补充核对数量级。",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verified steps=%#v, want ordered method-only steps %#v", got, want)
	}
}

func TestVerifiedFullSolutionStepsFallsBackToOneVerifiedElement(t *testing.T) {
	const solution = "解：4.5×2=9"
	if got := verifiedFullSolutionSteps(solution); !reflect.DeepEqual(got, []string{solution}) {
		t.Fatalf("verified steps fallback=%#v, want exact single verified element", got)
	}
}

func TestVerifiedFullSolutionStepsDoesNotCorruptLeadingDecimal(t *testing.T) {
	const solution = "4.5×2=9"
	if got := verifiedFullSolutionSteps(solution); !reflect.DeepEqual(got, []string{solution}) {
		t.Fatalf("leading decimal was mistaken for a list marker: %#v", got)
	}
}

func TestAnswerAnchorRejectsEmptyExplicitAnswerSection(t *testing.T) {
	const solution = "## 方法\n45×2=90，再点回小数点得到9。\n## 答案\n"
	if answerAnchoredInVerifiedSolution("90", solution) {
		t.Fatal("an empty explicit answer section must not fall back to an intermediate result")
	}
}
