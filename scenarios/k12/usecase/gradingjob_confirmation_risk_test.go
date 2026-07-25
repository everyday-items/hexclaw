package usecase

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestImageTaskPhotoGrading_ClearFormattedOCRAutoFreezesAndCompletes(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{
			Question: "4÷0.5=", Subject: "数学",
			AnswerState: AnswerStatePresent, StudentAnswer: "8",
			RecognitionConfidence: float64Ptr(0.99), OCRSignals: []string{"decimal_point"},
		},
		{
			Question: `\frac{5}{7}-\frac{1}{5}=`, Subject: "数学",
			AnswerState: AnswerStatePresent, StudentAnswer: `\frac{18}{35}`,
			RecognitionConfidence: float64Ptr(0.99), OCRSignals: []string{"fraction"},
		},
	}}
	d := recoveryDeps(t, rec, nil, nil)
	o := newRecoverableOrchestrator(t, d, t.TempDir())
	v, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "image_task", SourceKey: "clear-formatted-auto-freeze",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	v, err = o.RunGradingJob(ctx, jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("clear image-task facts must auto-freeze, got stage=%s confirmation=%s",
			v.Record.Status, v.Fields.ConfirmationState)
	}
	v = waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
	questions, ok := o.RecognizedQuestions(ctx, jobID)
	if !ok || len(questions) != 2 {
		t.Fatalf("missing recognized questions: %#v", questions)
	}
	for _, question := range questions {
		if question.ConfirmationRequired || question.ConfirmedVersion != 1 || question.InputDigest == "" {
			t.Fatalf("clear formatted fact was not auto-frozen: %#v", question)
		}
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 2 {
		t.Fatalf("program-verified grading did not run after auto-freeze: %#v", result)
	}
}

func TestImageTaskPhotoGrading_MissingConfidenceRequiresConfirmation(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "7+8=", Subject: "数学",
		AnswerState: AnswerStatePresent, StudentAnswer: "15",
	}}}
	d := recoveryDeps(t, rec, nil, nil)
	o := newRecoverableOrchestrator(t, d, t.TempDir())
	v, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "image_task", SourceKey: "missing-confidence",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	v, err = o.RunGradingJob(ctx, jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation ||
		v.Fields.ConfirmationState != k12.GradingConfirmationPending {
		t.Fatalf("missing confidence must stop for confirmation, got stage=%s confirmation=%s",
			v.Record.Status, v.Fields.ConfirmationState)
	}
	questions, ok := o.RecognizedQuestions(ctx, jobID)
	if !ok || len(questions) != 1 || !questions[0].ConfirmationRequired {
		t.Fatalf("missing confidence risk was not projected: %#v", questions)
	}
	hasLowConfidence := false
	for _, reason := range questions[0].ConfirmationReasons {
		hasLowConfidence = hasLowConfidence || reason == OCRRiskLowConfidence
	}
	if !hasLowConfidence {
		t.Fatalf("missing confidence must expose low_confidence: %#v", questions[0].ConfirmationReasons)
	}
}

func TestConfirmPhotoGradingJob_ConflictingOCRRequiresExplicitItemConfirmationAndPreservesRaw(t *testing.T) {
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
		EvidenceTranscriptions: []string{
			`3/5+1/5=`,
			`3/5+7/5=`,
		},
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
	hasConflict := false
	for _, reason := range qs[0].ConfirmationReasons {
		hasConflict = hasConflict || reason == OCRRiskEvidenceConflict
	}
	if !hasConflict {
		t.Fatalf("conflicting OCR must preserve evidence_conflict reason: %#v", qs[0].ConfirmationReasons)
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
