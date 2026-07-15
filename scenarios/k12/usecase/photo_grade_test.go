package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type photoRecognizerFake struct {
	questions []RecognizedQuestion
	err       error
}

func (f photoRecognizerFake) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	return f.questions, f.err
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
		fakeGrader{outcome: GradeOutcome{Correct: true}}, nil,
	)
	box := &BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "2", BBox: box},
		{Question: "2+2=", Subject: "数学", StudentAnswer: ""},
	}}
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
	if !strings.Contains(got.Markdown, "作业批改完成") || !strings.Contains(got.Markdown, "未作答") {
		t.Fatalf("grade markdown missing summary: %s", got.Markdown)
	}
}

func TestGradeHomeworkPhoto_VerifiedAnswersWithoutBBoxStillProduceNumberedCorrectionImage(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Correct: false}}, nil,
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
	if got.AnnotatedImage == nil || annotator.calls != 1 {
		t.Fatalf("verified answers must still produce a correction image when recognition has no safe bbox: image=%v calls=%d", got.AnnotatedImage, annotator.calls)
	}
	if len(annotator.marks) != 1 || annotator.marks[0].QuestionNumber != 1 {
		t.Fatalf("numbered fallback annotations = %#v, want question number 1", annotator.marks)
	}
	for _, mark := range annotator.marks {
		if mark.Correct || mark.BBox.W != 0 || mark.BBox.H != 0 {
			t.Fatalf("fallback mark must preserve wrong verdict without inventing coordinates: %#v", mark)
		}
	}
	if !strings.Contains(got.Markdown, "按题号标注") {
		t.Fatalf("markdown must explain the truthful numbered fallback: %s", got.Markdown)
	}
}

func TestGradeHomeworkPhoto_BlankSheetSolvesMarkdownWithoutFakeCorrectionImage(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "解：4.5×2=9", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Correct: true}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "4.5×2=", Subject: "数学"},
		{Question: "2+2=", Subject: "数学"},
	}}
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
	if !strings.Contains(got.Markdown, "作业解题") || !strings.Contains(got.Markdown, "4.5×2=9") {
		t.Fatalf("solve markdown missing content: %s", got.Markdown)
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
	d, _ := newPipeline(t, problemSolverFake{}, fakeGrader{outcome: GradeOutcome{Correct: false}}, nil)
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
	if err != nil {
		t.Fatal(err)
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

func TestTrustedPhotoMarks_DropsCollidingAnchorsInsteadOfMakingAmbiguousCluster(t *testing.T) {
	items := []PhotoGradeItem{
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.50, Y: 0.20, W: 0.10, H: 0.08}}, Status: PhotoCorrect},
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.54, Y: 0.21, W: 0.10, H: 0.08}}, Status: PhotoWrong},
		{Recognized: RecognizedQuestion{BBox: &BBox{X: 0.80, Y: 0.60, W: 0.10, H: 0.08}}, Status: PhotoCorrect},
	}

	marks := trustedPhotoMarks(items)
	if len(marks) != 1 || marks[0].BBox.X != 0.80 || !marks[0].Correct {
		t.Fatalf("colliding anchors must both degrade to text; got %#v", marks)
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
		"其余 5 题已在批改图右侧按题号标注",
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
		"1. **4÷0.5=** → **8**",
		"### ❌ 需要订正（1）",
		"#### 第 2 题",
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
