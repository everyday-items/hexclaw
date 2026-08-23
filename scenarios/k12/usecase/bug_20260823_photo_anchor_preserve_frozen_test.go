package usecase

import (
	"context"
	"errors"
	"testing"
)

type frozenAnswerBoundaryAnchorer struct {
	geometryCalls int
	fullCalls     int
	geometryErr   error
}

func (a *frozenAnswerBoundaryAnchorer) AnchorAnswerGeometry(
	_ context.Context,
	_ []byte,
	questions []RecognizedQuestion,
) ([]RecognizedQuestion, error) {
	a.geometryCalls++
	if a.geometryErr != nil {
		return nil, a.geometryErr
	}
	out := cloneRecognizedQuestions(questions)
	box := BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	out[0].BBox = &box
	// 几何 adapter 即使越权返回其它字段，也只能由 BBox 合并边界消费。
	out[0].Question = "被锚点改写的题干"
	out[0].StudentAnswer = "999"
	out[0].AnswerState = AnswerStateUnclear
	return out, nil
}

type fullTranscriptionOnlyAnchorer struct {
	fullCalls int
}

func (a *fullTranscriptionOnlyAnchorer) AnchorAnswers(
	_ context.Context,
	_ []byte,
	questions []RecognizedQuestion,
) ([]RecognizedQuestion, error) {
	a.fullCalls++
	out := cloneRecognizedQuestions(questions)
	out[0].Question = "被完整誊录改写的题干"
	out[0].StudentAnswer = ""
	out[0].AnswerState = AnswerStateUnclear
	return out, nil
}

func (a *frozenAnswerBoundaryAnchorer) AnchorAnswers(
	_ context.Context,
	_ []byte,
	questions []RecognizedQuestion,
) ([]RecognizedQuestion, error) {
	a.fullCalls++
	out := cloneRecognizedQuestions(questions)
	out[0].StudentAnswer = ""
	out[0].AnswerState = AnswerStateUnclear
	return out, nil
}

func TestREGBUGK12AnchorPreserveFrozen005DirectPhotoUsesGeometryOnly(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		ProblemID:     "problem-1",
		Question:      "1+1=",
		Subject:       "数学",
		StudentAnswer: "2",
		AnswerState:   AnswerStatePresent,
	}}}
	anchorer := &frozenAnswerBoundaryAnchorer{}
	d.AnswerAnchorer = anchorer
	d.PhotoAnnotator = &photoAnnotatorFake{}
	d.ParentTeachingGuide = &parentTeachingGuideSpy{}

	got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming",
		Grade:     "五年级上",
		Image:     []byte("jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if anchorer.geometryCalls != 1 || anchorer.fullCalls != 0 {
		t.Fatalf("冻结识题后只能调用几何锚点: geometry=%d full=%d",
			anchorer.geometryCalls, anchorer.fullCalls)
	}
	if len(got.Items) != 1 || got.Items[0].Recognized.Question != "1+1=" ||
		got.Items[0].Recognized.StudentAnswer != "2" ||
		got.Items[0].Recognized.AnswerState != AnswerStatePresent ||
		got.Items[0].Recognized.BBox == nil {
		t.Fatalf("锚点覆盖了冻结题干/作答事实，或未合并 BBox: %#v", got.Items)
	}
}

func TestREGBUGK12AnchorPreserveFrozen005DirectPhotoDegradesWithoutTranscriptionFallback(t *testing.T) {
	newDeps := func(t *testing.T) Deps {
		t.Helper()
		d, _ := newPipeline(t,
			fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
			fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
		)
		d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
			ProblemID:     "problem-1",
			Question:      "1+1=",
			Subject:       "数学",
			StudentAnswer: "2",
			AnswerState:   AnswerStatePresent,
		}}}
		d.PhotoAnnotator = &photoAnnotatorFake{}
		d.ParentTeachingGuide = &parentTeachingGuideSpy{}
		return d
	}

	t.Run("geometry failure preserves frozen facts", func(t *testing.T) {
		d := newDeps(t)
		anchorer := &frozenAnswerBoundaryAnchorer{geometryErr: errors.New("locator unavailable")}
		d.AnswerAnchorer = anchorer
		got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
			AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if anchorer.geometryCalls != 1 || anchorer.fullCalls != 0 || got.ImageWarning == "" {
			t.Fatalf("几何失败未显式降级或调用了完整誊录: geometry=%d full=%d warning=%q",
				anchorer.geometryCalls, anchorer.fullCalls, got.ImageWarning)
		}
		if len(got.Items) != 1 || got.Items[0].Recognized.Question != "1+1=" ||
			got.Items[0].Recognized.StudentAnswer != "2" ||
			got.Items[0].Recognized.AnswerState != AnswerStatePresent {
			t.Fatalf("几何失败改写了冻结事实: %#v", got.Items)
		}
	})

	t.Run("missing geometry capability never calls full transcription", func(t *testing.T) {
		d := newDeps(t)
		anchorer := &fullTranscriptionOnlyAnchorer{}
		d.AnswerAnchorer = anchorer
		got, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
			AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if anchorer.fullCalls != 0 || got.ImageWarning == "" {
			t.Fatalf("能力缺失时未显式降级或调用了完整誊录: full=%d warning=%q",
				anchorer.fullCalls, got.ImageWarning)
		}
		if len(got.Items) != 1 || got.Items[0].Recognized.Question != "1+1=" ||
			got.Items[0].Recognized.StudentAnswer != "2" ||
			got.Items[0].Recognized.AnswerState != AnswerStatePresent {
			t.Fatalf("能力缺失改写了冻结事实: %#v", got.Items)
		}
	})
}
