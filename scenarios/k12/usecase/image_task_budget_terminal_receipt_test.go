package usecase

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestImageTaskBudgetPreflightFailureConvergesToDurableSolveReceipt(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	grading.startErr = fmt.Errorf(
		"%w: public image_task requires a frozen grading budget policy",
		ErrInvalidInput,
	)

	view, created, runErr := createAndRunImageTask(
		t,
		coordinator,
		testCreateImageTaskInput(),
	)
	if !created || !errors.Is(runErr, ErrInvalidInput) {
		t.Fatalf("expected accepted dispatch and deterministic preflight failure: created=%v err=%v", created, runErr)
	}
	if grading.starts != 1 || grading.async != 0 {
		t.Fatalf("preflight must not launch async grading: starts=%d async=%d", grading.starts, grading.async)
	}

	result, err := coordinator.Result(
		context.Background(),
		"mingming",
		view.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Dispatch.Status != k12.ImageTaskStatusFailed ||
		!result.Dispatch.RetrySafe {
		t.Fatalf("dispatch did not converge to retry-safe failed: %+v", result.Dispatch)
	}
	if len(result.OperationReceipts) != 2 {
		t.Fatalf("expected classification and terminal solve receipts: %+v", result.OperationReceipts)
	}
	var receipt ImageTaskOperationReceipt
	for _, candidate := range result.OperationReceipts {
		if candidate.Operation == "solve" {
			receipt = candidate
		}
	}
	if receipt.InvocationID == "" ||
		receipt.Operation != "solve" ||
		receipt.CanonicalInputDigest != view.Dispatch.SourceDigest ||
		receipt.Status != string(k12.ImageTaskInvocationFailed) ||
		receipt.Attempt != 1 ||
		receipt.Provider != "hexclaw-gpt" ||
		receipt.Model != "gpt-5.6-sol" ||
		receipt.ResultDigest != "" {
		t.Fatalf("unexpected terminal solve receipt: %+v", receipt)
	}

	submission, err := coordinator.Records.GetHomeworkSubmission(
		context.Background(),
		"mingming",
		result.Dispatch.TargetObjectID,
	)
	if err != nil {
		t.Fatalf("GetHomeworkSubmission: %v", err)
	}
	if submission.Status != k12.HomeworkSubmissionFailed ||
		submission.GradingJobID != "" {
		t.Fatalf("homework did not fail before GradingJob creation: %+v", submission)
	}

	replayed, err := coordinator.Result(
		context.Background(),
		"mingming",
		view.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatalf("replayed Result: %v", err)
	}
	if !reflect.DeepEqual(replayed.OperationReceipts, result.OperationReceipts) {
		t.Fatalf(
			"terminal receipt identity drifted: first=%+v replay=%+v",
			result.OperationReceipts,
			replayed.OperationReceipts,
		)
	}
}
