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
