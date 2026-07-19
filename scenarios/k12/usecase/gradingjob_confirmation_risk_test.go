package usecase

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestConfirmPhotoGradingJob_RiskyOCRRequiresExplicitItemConfirmationAndPreservesRaw(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw := `原图转写：\frac{3}{5}+\frac{1}{5}=`
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question:               "3/5+1/5=",
		RawTranscription:       raw,
		CanonicalMarkdown:      `$\frac{3}{5}+\frac{1}{5}=$`,
		Subject:                "数学",
		AnswerState:            AnswerStatePresent,
		StudentAnswer:          "3/5",
		AnswerRawTranscription: "3/5",
		RecognitionConfidence:  float64Ptr(0.99),
	}}}
	d := recoveryDeps(t, rec, &photoAnchorerFake{boxes: map[int]BBox{0: {X: .2, Y: .3, W: .1, H: .05}}}, &photoAnnotatorFake{})
	o := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "risk-confirm",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	if _, err = o.RunGradingJob(ctx, jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation && v.Fields.AnchorState == k12.GradingAnchorLocated
	})
	qs, ok := o.RecognizedQuestions(ctx, jobID)
	if !ok || len(qs) != 1 || !qs[0].ConfirmationRequired || qs[0].ProblemID == "" {
		t.Fatalf("recognition must expose stable risky item: %#v", qs)
	}

	_, handled, err := o.ConfirmPhotoGradingJob(ctx, jobID, ConfirmPhotoGradingInput{})
	if !handled || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("risk without explicit per-item confirmation must fail closed: handled=%v err=%v", handled, err)
	}
	v, err = d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil || v.Fields.ConfirmationState != k12.GradingConfirmationPending {
		t.Fatalf("rejected confirmation must leave job pending: view=%#v err=%v", v, err)
	}

	_, handled, err = o.ConfirmPhotoGradingJob(ctx, jobID, ConfirmPhotoGradingInput{
		Corrections: []GradingQuestionCorrection{{
			ProblemID: qs[0].ProblemID, Index: 0, Confirmed: true,
			CanonicalMarkdown:       `$\frac{3}{5}+\frac{1}{5}=$`,
			AnswerCanonicalMarkdown: `$\frac{4}{5}$`,
			AnswerState:             AnswerStatePresent,
		}},
	})
	if err != nil || !handled {
		t.Fatalf("explicit item confirmation: handled=%v err=%v", handled, err)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 1 {
		t.Fatalf("missing result: %#v", result)
	}
	got := result.Items[0].Recognized
	if got.RawTranscription != raw || got.AnswerRawTranscription != "3/5" {
		t.Fatalf("parent correction must not overwrite OCR raw facts: %#v", got)
	}
	if got.AnswerCanonicalMarkdown != `$\frac{4}{5}$` || got.StudentAnswer != `$\frac{4}{5}$` {
		t.Fatalf("assessment must use confirmed canonical answer: %#v", got)
	}
	if got.ConfirmedVersion != 1 || got.InputDigest == "" || got.AttemptID == "" || got.PageAssetID == "" {
		t.Fatalf("confirmed attempt facts incomplete: %#v", got)
	}

	// 投递后的运行时清理不得删除 raw/canonical 审计事实；原图临时载体应被回收。
	o.ReleaseGradingRun(jobID)
	if _, err := os.Stat(o.runPath(jobID, "image.bin")); !os.IsNotExist(err) {
		t.Fatalf("terminal image should be cleaned, stat err=%v", err)
	}
	if _, err := os.Stat(o.recognitionAuditPath(jobID)); err != nil {
		t.Fatalf("recognition audit must survive terminal cleanup: %v", err)
	}
	archived, ok := o.RecognizedQuestions(ctx, jobID)
	if !ok || len(archived) != 1 || archived[0].RawTranscription != raw || archived[0].AnswerRawTranscription != "3/5" {
		t.Fatalf("archived raw facts unavailable: %#v", archived)
	}
}

func TestConfirmPhotoGradingJob_InvalidCanonicalNeedsCorrectionAndNeverPersistsPartialMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		RawTranscription: "原始题干 1/2", CanonicalMarkdown: `$\frac{1}{2$`,
		Subject: "数学", AnswerState: AnswerStateBlank,
	}}}
	d := recoveryDeps(t, rec, nil, nil)
	o := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "invalid-canonical",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID := v.Record.RecordID
	if _, err = o.RunGradingJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	qs, _ := o.RecognizedQuestions(ctx, jobID)
	if len(qs) != 1 || RecognizedQuestionDisplayText(qs[0]) != "原始题干 1/2" {
		t.Fatalf("invalid canonical must still be readable: %#v", qs)
	}

	_, _, err = o.ConfirmPhotoGradingJob(ctx, jobID, ConfirmPhotoGradingInput{
		Corrections: []GradingQuestionCorrection{{
			ProblemID: qs[0].ProblemID, Confirmed: true,
			CanonicalMarkdown: `$\frac{1}{2$`,
		}},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid corrected canonical must be rejected, got %v", err)
	}
	after, _ := o.RecognizedQuestions(ctx, jobID)
	if after[0].CanonicalMarkdown != `$\frac{1}{2$` || after[0].ConfirmedVersion != 0 {
		t.Fatalf("failed command must not partially freeze canonical state: %#v", after[0])
	}
}
