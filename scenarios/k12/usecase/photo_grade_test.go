package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
)

type photoRecognizerFake struct {
	questions []RecognizedQuestion
	err       error
}

func (f photoRecognizerFake) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	return f.questions, f.err
}

type photoAnchorerFake struct {
	boxes map[int]BBox
	err   error
	calls int
}

func (f *photoAnchorerFake) AnchorAnswers(
	_ context.Context,
	_ []byte,
	questions []RecognizedQuestion,
) ([]RecognizedQuestion, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := append([]RecognizedQuestion(nil), questions...)
	for index, bbox := range f.boxes {
		if index >= 0 && index < len(out) {
			box := bbox
			out[index].BBox = &box
		}
	}
	return out, nil
}

type photoAnnotatorFake struct {
	mu    sync.Mutex
	calls int
	marks []PhotoAnnotation
}

func (f *photoAnnotatorFake) Annotate(_ context.Context, _ []byte, marks []PhotoAnnotation) (RenderedPhoto, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.marks = append([]PhotoAnnotation(nil), marks...)
	return RenderedPhoto{Data: []byte("png"), MIME: "image/png"}, nil
}

func TestGradeHomeworkPhoto_AnsweredSheetGradesAndAnnotatesTrustedBBox(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "2"},
		{Question: "2+2=", Subject: "数学", StudentAnswer: ""},
	}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}
	d.AnswerAnchorer = anchorer
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "dt-1", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != PhotoModeGrade || len(got.Items) != 2 {
		t.Fatalf("mode/items = %q/%d, want grade/2", got.Mode, len(got.Items))
	}
	if got.Items[0].Status != PhotoCorrect || got.Items[1].Status != PhotoUnanswered {
		t.Fatalf("unexpected statuses: %#v", got.Items)
	}
	if got.AnnotatedImage == nil || string(got.AnnotatedImage.Data) != "png" {
		t.Fatalf("trusted answered bbox should produce annotated PNG: %#v", got.AnnotatedImage)
	}
	if annotator.calls != 1 || len(annotator.marks) != 1 || !annotator.marks[0].Correct {
		t.Fatalf("annotator calls/marks = %d/%#v", annotator.calls, annotator.marks)
	}
	if anchorer.calls != 1 {
		t.Fatalf("photo grading must invoke the page-batch answer anchorer once, calls=%d", anchorer.calls)
	}
	if !strings.Contains(got.Markdown, "作业批改完成") || !strings.Contains(got.Markdown, "未作答") {
		t.Fatalf("grade markdown missing summary: %s", got.Markdown)
	}
}

func TestGradeHomeworkPhotoFrozenDispatchIntentFailsClosedOnRecognitionMismatch(t *testing.T) {
	tests := []struct {
		name      string
		intent    PhotoTaskIntent
		answer    string
		wantError string
	}{
		{
			name:   "completed dispatch but recognition has no answer evidence",
			intent: PhotoTaskCompletedHomework, answer: "",
			wantError: "与识别到的作答证据冲突",
		},
		{
			name:   "blank dispatch but recognition has answer evidence",
			intent: PhotoTaskBlankWorksheet, answer: "2",
			wantError: "与识别到的作答证据冲突",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newPipeline(t,
				fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
				fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
			)
			d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
				Question: "1+1=", Subject: "数学", StudentAnswer: tt.answer,
			}}}
			_, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
				AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
				TaskIntent: tt.intent,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("mismatch must fail closed, got %v", err)
			}
		})
	}
}

func TestGradeHomeworkPhoto_CompoundParentIsNotAssessedAndChildrenStayIndependent(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "答案", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{ProblemID: "parent-1", ProblemKind: ProblemKindCompoundParent, Question: "阅读短文《春天》", Subject: "语文"},
		{ProblemID: "child-1", ProblemKind: ProblemKindSubproblem, ParentProblemID: "parent-1", SubproblemNo: "1", Question: "写出中心句", Subject: "语文", StudentAnswer: "春天来了"},
		{ProblemID: "child-2", ProblemKind: ProblemKindSubproblem, ParentProblemID: "parent-1", SubproblemNo: "2", Question: "概括第二段", Subject: "语文"},
	}}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级下", SourceSession: "dt-compound", Image: []byte("compound-photo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("compound parent must not create assessment item: %#v", got.Items)
	}
	firstID := got.Items[0].Recognized.ProblemID
	secondID := got.Items[1].Recognized.ProblemID
	firstParentID := got.Items[0].Recognized.ParentProblemID
	secondParentID := got.Items[1].Recognized.ParentProblemID
	if firstID == "" || secondID == "" || firstID == secondID || firstID == "child-1" || secondID == "child-2" ||
		firstParentID == "" || firstParentID != secondParentID || firstParentID == "parent-1" {
		t.Fatalf("siblings need independent server identity under one server parent: %#v", got.Items)
	}
	for i, item := range got.Items {
		if !strings.Contains(item.Recognized.Question, "阅读短文《春天》") {
			t.Fatalf("child %d assessment input lacks shared stem: %q", i, item.Recognized.Question)
		}
	}
	if got.Items[0].Recognized.StudentAnswer != "春天来了" || got.Items[1].Recognized.StudentAnswer != "" {
		t.Fatalf("one sibling answer overwrote another: %#v", got.Items)
	}
}

func TestGradeHomeworkPhoto_VerifiedAnswersWithoutBBoxRemainTextOnlyInsteadOfSendingUnchangedPhoto(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "dt-no-bbox", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AnnotatedImage != nil || annotator.calls != 0 {
		t.Fatalf("verified answers without safe coordinates must not send an unchanged photo as a correction image: image=%v calls=%d", got.AnnotatedImage, annotator.calls)
	}
	for _, want := range []string{"1 题已判定", "0 题已在图上标注", "未生成批改图", "仅作文字汇总"} {
		if !strings.Contains(got.Markdown, want) {
			t.Fatalf("text-only fallback must explain missing safe coordinates; missing %q in:\n%s", want, got.Markdown)
		}
	}
}

func TestGradeHomeworkPhoto_PartialCoordinatesOnlyPassTrustedMarksToAnnotator(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "2"},
		{Question: "2+2=", Subject: "数学", StudentAnswer: "4"},
	}}
	d.AnswerAnchorer = &photoAnchorerFake{boxes: map[int]BBox{
		0: {X: 0.20, Y: 0.30, W: 0.15, H: 0.08},
	}}
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AnnotatedImage == nil || annotator.calls != 1 {
		t.Fatalf("the safely located answer should still produce one correction image: image=%v calls=%d", got.AnnotatedImage, annotator.calls)
	}
	if len(annotator.marks) != 1 || annotator.marks[0].QuestionNumber != 1 || !photoAnnotationHasTrustedBBox(annotator.marks[0]) {
		t.Fatalf("annotator received unpositioned or wrong marks: %#v", annotator.marks)
	}
}

func TestGradeHomeworkPhoto_FifthGradeFractionOfQuantityIsNotRejectedByCoarseVisionLabel(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "8/5", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Constraint = curriculum.New()
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question:        "8的1/4的4/5是多少？",
		Subject:         "数学",
		KnowledgePoints: []string{"分数乘法"},
		StudentAnswer:   "8/5",
	}}}
	d.PhotoAnnotator = &photoAnnotatorFake{}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级下", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Status != PhotoCorrect {
		t.Fatalf("fifth-grade fraction-of-quantity problem was falsely rejected as out of scope: %#v", got.Items)
	}
}

func TestGradeHomeworkPhoto_FifthGradeChineseFractionWordingIsNotRejectedByCoarseVisionLabel(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "10", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Constraint = curriculum.New()
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question:        "8的四分之五是多少？",
		Subject:         "数学",
		KnowledgePoints: []string{"分数乘法"},
		StudentAnswer:   "10",
	}}}
	d.PhotoAnnotator = &photoAnnotatorFake{}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级下", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Status != PhotoCorrect {
		t.Fatalf("Chinese fraction-of-quantity wording was falsely rejected as out of scope: %#v", got.Items)
	}
}

func TestGradeHomeworkPhoto_FifthGradeUnknownWholeFractionProblemIsNotRejectedAsFormalFractionDivision(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "64", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Constraint = curriculum.New()
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question:        "一个数的3/8是24，求这个数？",
		Subject:         "数学",
		KnowledgePoints: []string{"分数除法"},
		StudentAnswer:   "64",
	}}}
	d.PhotoAnnotator = &photoAnnotatorFake{}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级下", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Status != PhotoCorrect {
		t.Fatalf("fifth-grade unknown-whole fraction problem was falsely rejected as formal fraction division: %#v", got.Items)
	}
}

func TestGradeHomeworkPhoto_BlankSheetSolvesMarkdownWithoutFakeCorrectionImage(t *testing.T) {
	d, _ := newPipeline(t,
		blankWorksheetSolver{},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "4.5×2=", Subject: "数学"},
		{Question: "2+2=", Subject: "数学"},
	}}
	d.ParentTeachingGuide = &parentTeachingGuideSpy{}
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != PhotoModeSolve || got.AnnotatedImage != nil || annotator.calls != 0 {
		t.Fatalf("blank sheet must be solve-only without annotated image: %+v calls=%d", got, annotator.calls)
	}
	if len(got.Items) != 2 || got.Items[0].Status != PhotoBlankSolved || got.Items[1].Status != PhotoBlankSolved {
		t.Fatalf("blank items should be solved: %#v", got.Items)
	}
	if !strings.Contains(got.Markdown, "家长辅导指南") ||
		!strings.Contains(got.Markdown, "4.5×2=") ||
		!strings.Contains(got.Markdown, "**答案：** 9") {
		t.Fatalf("solve markdown missing content: %s", got.Markdown)
	}
}

func TestGradeHomeworkPhoto_AnswerRegionWithoutReadableTextFailsClosedInsteadOfSolving(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "解：4.5×2=9", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question:      "4.5×2=",
		Subject:       "数学",
		AnswerState:   AnswerStateUnclear,
		StudentAnswer: "",
	}}}
	d.AnswerAnchorer = &photoAnchorerFake{boxes: map[int]BBox{
		0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05},
	}}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != PhotoModeGrade {
		t.Fatalf("a located answer region with unreadable text must fail closed as grade mode, got %q", got.Mode)
	}
	if len(got.Items) != 1 || got.Items[0].Status != PhotoAnswerUnclear {
		t.Fatalf("ambiguous handwriting must be reported for review instead of receiving a generated solution: %#v", got.Items)
	}
	if strings.Contains(got.Markdown, "作业解题") || strings.Contains(got.Markdown, "4.5×2=9") {
		t.Fatalf("ambiguous answered sheet leaked a solution:\n%s", got.Markdown)
	}
}

func TestGradeHomeworkPhoto_ModelOnlyUnclearWithoutVerifiedInkIsBlankSolve(t *testing.T) {
	d, _ := newPipeline(t,
		blankWorksheetSolver{},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{
			Question:      "4.5×2=",
			Subject:       "数学",
			AnswerState:   AnswerStateUnclear,
			StudentAnswer: "",
		},
		{
			Question:    "2+2=",
			Subject:     "数学",
			AnswerState: AnswerStateBlank,
		},
	}}
	anchorer := &photoAnchorerFake{}
	d.AnswerAnchorer = anchorer
	d.ParentTeachingGuide = &parentTeachingGuideSpy{}
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if anchorer.calls != 1 {
		t.Fatalf("model-only unclear candidates require one independent ink-verification pass, calls=%d", anchorer.calls)
	}
	if got.Mode != PhotoModeSolve {
		t.Fatalf("unverified model-only unclear state must not turn a blank worksheet into grade mode, got %q", got.Mode)
	}
	if got.AnnotatedImage != nil || annotator.calls != 0 {
		t.Fatalf("blank worksheet must not produce a fake correction image: image=%v calls=%d", got.AnnotatedImage, annotator.calls)
	}
	for i, item := range got.Items {
		if item.Status != PhotoBlankSolved {
			t.Fatalf("blank item %d was not solved after ink verification: %#v", i+1, item)
		}
	}
	if !strings.Contains(got.Markdown, "家长辅导指南") ||
		!strings.Contains(got.Markdown, "4.5×2=") ||
		!strings.Contains(got.Markdown, "**答案：** 9") {
		t.Fatalf("blank solve markdown missing solution:\n%s", got.Markdown)
	}
}

type problemSolverFake struct{}

func (problemSolverFake) Solve(_ context.Context, problem, _, _ string) (SolveResult, error) {
	if strings.Contains(problem, "失败") {
		return SolveResult{}, errors.New("upstream timeout")
	}
	return SolveResult{Solution: "2", Evidence: SolveEvidence{Verdict: VerdictUnverifiable, EvidenceType: EvidenceNone}}, nil
}

func TestGradeHomeworkPhoto_UntrustedOrFailedItemNeverBurnsRedCross(t *testing.T) {
	d, _ := newPipeline(t, problemSolverFake{}, fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree}}, nil)
	box := &BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3", BBox: box},
		{Question: "失败题", Subject: "数学", StudentAnswer: "1", BBox: box},
	}}
	annotator := &photoAnnotatorFake{}
	d.PhotoAnnotator = annotator

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if err == nil {
		t.Fatal("failed item must keep the page incomplete while retaining diagnostic output")
	}
	if got.AnnotatedImage != nil || annotator.calls != 0 {
		t.Fatalf("untrusted/failed results must not be burned into image: image=%v calls=%d", got.AnnotatedImage, annotator.calls)
	}
	if got.Items[0].Status != PhotoUntrusted || got.Items[1].Status != PhotoFailed {
		t.Fatalf("unexpected statuses: %#v", got.Items)
	}
	if !strings.Contains(got.Markdown, "待核对") {
		t.Fatalf("markdown must explain degraded verification: %s", got.Markdown)
	}
}

func TestTrustedPhotoMarks_RejectsUnverifiedAnnotationAnchor(t *testing.T) {
	items := []PhotoGradeItem{
		{Recognized: RecognizedQuestion{Question: "4÷0.5=", StudentAnswer: "8"}, Status: PhotoCorrect},
		{Recognized: RecognizedQuestion{Question: "0.5+1/3=", StudentAnswer: "2/3"}, Status: PhotoWrong},
		{Recognized: RecognizedQuestion{Question: "待核对", StudentAnswer: "?"}, Status: PhotoUntrusted},
	}

	marks := trustedPhotoMarks(items)
	if len(marks) != 0 {
		t.Fatalf("semantic-rejected coordinates must never be drawn, got %#v", marks)
	}
}

func TestTrustedPhotoMarks_RetainsEveryVerifiedAnchorRegardlessOfNormalizedProximity(t *testing.T) {
	items := []PhotoGradeItem{
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.50, Y: 0.20, W: 0.10, H: 0.08}}, Status: PhotoCorrect},
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.54, Y: 0.21, W: 0.10, H: 0.08}}, Status: PhotoWrong},
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.80, Y: 0.60, W: 0.10, H: 0.08}}, Status: PhotoCorrect},
	}

	marks := trustedPhotoMarks(items)
	if len(marks) != len(items) {
		t.Fatalf("domain layer must not discard verified anchors using normalized-distance guesses: %#v", marks)
	}
}

func TestPhotoGradeMarkdown_PartialTrustedCoordinatesExplainsImageCoverage(t *testing.T) {
	box := &BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	box2 := &BBox{X: 0.7, Y: 0.6, W: 0.1, H: 0.05}
	result := PhotoGradeResult{
		Mode:           PhotoModeGrade,
		AnnotatedImage: &RenderedPhoto{Data: []byte("png"), MIME: "image/png"},
		Items: []PhotoGradeItem{
			{Recognized: RecognizedQuestion{Question: "答对题不要逐题展开", BBox: box}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "答错题一", StudentAnswer: "1", BBox: box2}, Status: PhotoWrong},
			{Recognized: RecognizedQuestion{Question: "答对题二"}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "答对题三"}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "答对题四"}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "答对题五"}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "答错题二", StudentAnswer: "2"}, Status: PhotoWrong},
		},
	}

	got := photoGradeMarkdown(result)
	for _, want := range []string{
		"7 题已判定",
		"2 题在原作答位置标注",
		"其余 5 题仅作文字汇总",
		"### ✅ 答对的题（5）",
		"**答对题不要逐题展开**",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("partial annotation markdown missing %q:\n%s", want, got)
		}
	}
}

func TestPhotoGradeMarkdown_NoActualAnnotationExplainsImageFallback(t *testing.T) {
	box := &BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	result := PhotoGradeResult{
		Mode: PhotoModeGrade,
		Items: []PhotoGradeItem{
			{Recognized: RecognizedQuestion{Question: "题一", BBox: box}, Status: PhotoCorrect},
			{Recognized: RecognizedQuestion{Question: "题二"}, Status: PhotoWrong},
		},
	}

	got := photoGradeMarkdown(result)
	for _, want := range []string{"2 题已判定", "0 题已在图上标注", "未生成批改图", "仅作文字汇总，以避免标记错位"} {
		if !strings.Contains(got, want) {
			t.Fatalf("zero annotation markdown missing %q:\n%s", want, got)
		}
	}
}

func TestPhotoGradeMarkdown_DingtalkUsesStructuredCorrectAndCorrectionSections(t *testing.T) {
	result := PhotoGradeResult{
		Mode: PhotoModeGrade,
		Items: []PhotoGradeItem{
			{Recognized: RecognizedQuestion{Question: "4÷0.5=", StudentAnswer: "8"}, Status: PhotoCorrect},
			{
				Recognized: RecognizedQuestion{Question: "0.5+1/3=", StudentAnswer: "2/3"}, Status: PhotoWrong,
				Grade: GradeResult{
					Solution: "## 解答\n0.5+1/3=1/2+1/3=5/6\n## 答案\n**5/6**",
					Outcome:  GradeOutcome{ErrorCause: "异分母分数未通分"},
				},
			},
		},
	}

	got := photoGradeMarkdown(result)
	for _, want := range []string{
		"## 📊 作业批改",
		"### ✅ 答对的题（1）",
		"- **4÷0.5=** → **8**",
		"### ❌ 需要订正（1）",
		"#### 0.5+1/3=",
		"- **题目：** 0.5+1/3=",
		"- **你的作答：** 2/3",
		"- **订正参考：**\n\n> 解答  \n> 0.5+1/3=1/2+1/3=5/6  \n> 答案  \n> **5/6**",
		"- **错因：** 异分母分数未通分",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DingTalk Markdown missing %q:\n%s", want, got)
		}
	}
}
