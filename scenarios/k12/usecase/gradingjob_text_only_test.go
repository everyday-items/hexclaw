package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// A text-only run must never inherit the photo compositor, even if a restored
// or legacy checkpoint unexpectedly contains trusted coordinates. The worker's
// deterministic pipeline token is not user media and must not cross that port.
func TestGradingOrchestratorTextOnlyNeverCallsPhotoAnnotator(t *testing.T) {
	ctx := context.Background()
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{
		0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05},
	}}
	annotator := &photoAnnotatorFake{}
	orchestrator := newOrchestrator(t, recognizer, anchorer, annotator)

	view := startOrchestratorJob(t, orchestrator, "text-only-annotator-guard")
	jobID := view.Record.RecordID
	if _, err := orchestrator.RunGradingJob(ctx, jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	waitGradingView(t, orchestrator, jobID, func(view GradingJobView) bool {
		return view.Fields.AnchorState == k12.GradingAnchorLocated
	})

	run := orchestrator.lookup(jobID)
	if run == nil {
		t.Fatal("grading run missing")
	}
	run.textOnly = true
	if _, err := orchestrator.ConfirmAndRun(ctx, jobID, nil); err != nil {
		t.Fatalf("ConfirmAndRun: %v", err)
	}
	if annotator.calls != 0 {
		t.Fatalf("text-only run crossed PhotoAnnotator port %d time(s)", annotator.calls)
	}
	result, ok := orchestrator.PhotoResult(jobID)
	if !ok || result.AnnotatedImage != nil {
		t.Fatalf("text-only result must remain text-only: ok=%v image=%#v", ok, result.AnnotatedImage)
	}
}
