package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// retryableImageTaskGradingStub models a durable child grading Job whose
// provider call definitely failed before producing a result. It deliberately
// exposes the generic runtime retry seam while declining the narrowly scoped
// parent-window retry used only for interactive deadline expiry.
type retryableImageTaskGradingStub struct {
	imageTaskGradingStub

	projection     ImageTaskHomeworkProjection
	genericCalls   int
	genericHandled bool
}

func (s *retryableImageTaskGradingStub) ImageTaskHomeworkProjection(
	_ context.Context,
	_ string,
	jobID string,
) (ImageTaskHomeworkProjection, error) {
	if jobID != s.resolvedJobID() {
		return ImageTaskHomeworkProjection{}, context.Canceled
	}
	return s.projection, nil
}

func (s *retryableImageTaskGradingStub) RetryPhotoGradingJob(
	_ context.Context,
	jobID string,
) (GradingJobView, bool, error) {
	if jobID != s.resolvedJobID() {
		return GradingJobView{}, false, context.Canceled
	}
	s.genericCalls++
	return GradingJobView{
		Record: &records.AgentRecord{RecordID: jobID},
	}, s.genericHandled, nil
}

// K12-IMAGE-TASK-RETRYABLE-RETRY-001: a definite provider failure recorded as
// failed_retryable/retryable must use the generic child Job retry path. The
// parent automatic window is only refreshed for interactive_deadline_exceeded;
// refreshing it for an HTTP 5xx would alter the frozen attempt unnecessarily.
func TestBUG20260802ImageTaskRetryableGradingUsesGenericRetryWithoutParentWindowRefresh(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	grading := &retryableImageTaskGradingStub{
		imageTaskGradingStub: imageTaskGradingStub{jobID: "grading-retryable-provider-502"},
		projection: ImageTaskHomeworkProjection{
			Stage:     k12.GradingStageFailedRetryable,
			Retryable: true,
		},
		genericHandled: true,
	}
	coordinator.Grading = grading

	created, fresh, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !fresh {
		t.Fatalf("create/run image task: fresh=%v err=%v", fresh, err)
	}
	before := created.Dispatch

	retried, err := coordinator.Retry(
		context.Background(),
		"mingming",
		before.DispatchID,
		before.Version,
	)
	if err != nil {
		t.Fatalf("retryable child grading failure must be retried: %v", err)
	}
	if grading.genericCalls != 1 {
		t.Fatalf("generic child grading retry calls=%d, want 1", grading.genericCalls)
	}
	if grading.parentRetryAttemptID != "" || grading.parentRetryDeadlineAt != 0 {
		t.Fatalf(
			"ordinary provider failure must not refresh parent automatic window: attempt=%q deadline=%d",
			grading.parentRetryAttemptID,
			grading.parentRetryDeadlineAt,
		)
	}
	if retried.Dispatch.DispatchID != before.DispatchID ||
		retried.Dispatch.Version != before.Version ||
		retried.Dispatch.AutomaticStartedAt != before.AutomaticStartedAt ||
		retried.Dispatch.AutomaticDeadlineAt != before.AutomaticDeadlineAt {
		t.Fatalf("ordinary retry changed dispatch automatic window: before=%+v after=%+v", before, retried.Dispatch)
	}
}

// K12-IMAGE-TASK-RETRYABLE-RETRY-001: after a restart, a process-local
// runtime may have no registered run. The public facade must still use the
// same durable child Job state machine and must not require a fresh upload,
// dispatch, or parent-window reset merely to make the retry available.
func TestBUG20260802ImageTaskRetryableGradingFallsBackToDurableJobWhenRuntimeHasNoRun(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	deps := coordinator.WorkFeedback.(*Deps)
	job, createdJob, err := deps.CreateGradingJob(
		context.Background(),
		"mingming",
		"session-retryable-no-runtime",
		CreateGradingJobInput{
			SubmissionID: "submission-retryable-no-runtime",
			SourceKind:   "test",
			SourceKey:    "image-task-retryable-no-runtime",
			ModelSnapshot: k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				Capability:               "vision",
				RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
			},
		},
	)
	if err != nil || !createdJob {
		t.Fatalf("create durable grading job: created=%v err=%v", createdJob, err)
	}
	failed, err := deps.AdvanceGradingStage(
		context.Background(),
		"mingming",
		job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:     GradingOutcomeFailed,
			FailureKind: "provider_response_http_502",
			Retryable:   true,
		},
	)
	if err != nil || failed.Record.Status != k12.GradingStageFailedRetryable || !failed.Fields.Retryable {
		t.Fatalf("prepare durable retryable job: view=%+v err=%v", failed, err)
	}
	grading := &retryableImageTaskGradingStub{
		imageTaskGradingStub: imageTaskGradingStub{jobID: job.Record.RecordID},
		projection: ImageTaskHomeworkProjection{
			Stage:     k12.GradingStageFailedRetryable,
			Retryable: true,
		},
		genericHandled: false,
	}
	coordinator.Grading = grading

	created, fresh, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !fresh {
		t.Fatalf("create/run image task: fresh=%v err=%v", fresh, err)
	}
	before := created.Dispatch
	if _, err := coordinator.Retry(
		context.Background(),
		"mingming",
		before.DispatchID,
		before.Version,
	); err != nil {
		t.Fatalf("retryable child grading without runtime must use durable fallback: %v", err)
	}
	if grading.genericCalls != 1 {
		t.Fatalf("generic runtime retry calls=%d, want 1 before durable fallback", grading.genericCalls)
	}
	resumed, err := deps.GetGradingJob(context.Background(), "mingming", job.Record.RecordID)
	if err != nil || resumed.Record.Status != k12.GradingStageQueued {
		t.Fatalf("durable retry must return same Job to queued: view=%+v err=%v", resumed, err)
	}
	if resumed.Record.RecordID != job.Record.RecordID {
		t.Fatalf("durable retry created a different Job: before=%q after=%q", job.Record.RecordID, resumed.Record.RecordID)
	}
}

// K12-IMAGE-TASK-RETRYABLE-RETRY-001: interactive deadline expiry is the
// single exception that may refresh a parent automatic window. If its
// specialized preflight declines, the facade must fail closed instead of
// silently using the ordinary generic retry path.
func TestBUG20260802ImageTaskInteractiveDeadlineNeverFallsBackToGenericRetry(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	grading := &retryableImageTaskGradingStub{
		imageTaskGradingStub: imageTaskGradingStub{jobID: "grading-interactive-deadline"},
		projection: ImageTaskHomeworkProjection{
			Stage:            k12.GradingStageFailedRetryable,
			Retryable:        true,
			retryFailureKind: gradingFailureInteractiveDeadlineExceeded,
		},
		genericHandled: true,
	}
	coordinator.Grading = grading
	created, fresh, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !fresh {
		t.Fatalf("create/run image task: fresh=%v err=%v", fresh, err)
	}
	if _, err := coordinator.Retry(
		context.Background(),
		"mingming",
		created.Dispatch.DispatchID,
		created.Dispatch.Version,
	); err == nil {
		t.Fatal("interactive deadline without parent-window permission must fail closed")
	}
	if grading.genericCalls != 0 {
		t.Fatalf("interactive deadline must not use generic retry: calls=%d", grading.genericCalls)
	}
}
