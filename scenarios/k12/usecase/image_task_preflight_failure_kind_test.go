package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type imageTaskPreflightFailureStarter struct {
	*GradingOrchestrator
	mutateInput func(*StartPhotoGradingInput)
	starts      int
	async       int
}

type imageTaskPreflightDeliverySpy struct{ calls int }

func (s *imageTaskPreflightDeliverySpy) PrepareText(
	context.Context,
	string,
	string,
) (PreparedTextDelivery, error) {
	s.calls++
	return PreparedTextDelivery{}, nil
}

func (s *imageTaskPreflightDeliverySpy) SendPrepared(
	context.Context,
	k12.DeliveryReceipt,
) (DeliveryTransportAck, error) {
	s.calls++
	return DeliveryTransportAck{}, nil
}

func (s *imageTaskPreflightDeliverySpy) QueryPrepared(
	context.Context,
	k12.DeliveryReceipt,
) (DeliveryTransportAck, error) {
	s.calls++
	return DeliveryTransportAck{}, nil
}

func (s *imageTaskPreflightFailureStarter) StartPhotoGradingJob(
	ctx context.Context,
	in StartPhotoGradingInput,
) (GradingJobView, bool, error) {
	s.starts++
	if s.mutateInput != nil {
		s.mutateInput(&in)
	}
	return s.GradingOrchestrator.StartPhotoGradingJob(ctx, in)
}

func (s *imageTaskPreflightFailureStarter) StartAsync(string) bool {
	s.async++
	return true
}

func TestImageTaskGradingPreflightFailureKindUsesTypedCause(t *testing.T) {
	validBudget := frozenWiringBudget()
	tests := []struct {
		name         string
		budget       k12.GradingBudgetSnapshot
		mutateInput  func(*StartPhotoGradingInput)
		wantKind     string
		wantIdentity error
	}{
		{
			name:         "missing frozen grading budget",
			wantKind:     "grading_budget_missing",
			wantIdentity: errGradingBudgetMissing,
		},
		{
			name: "structurally invalid grading budget",
			budget: k12.GradingBudgetSnapshot{
				PolicyVersion: 1,
			},
			wantKind:     "grading_budget_policy_invalid",
			wantIdentity: errGradingBudgetPolicyInvalid,
		},
		{
			name:   "invalid recognizing request policy",
			budget: validBudget,
			mutateInput: func(in *StartPhotoGradingInput) {
				in.ModelSnapshot.RecognizingRequestPolicy = k12.ModelRequestPolicySnapshot{}
			},
			wantKind:     "grading_model_request_policy_invalid",
			wantIdentity: ErrModelRequestPolicyInvalid,
		},
		{
			name:   "other invalid grading request",
			budget: validBudget,
			mutateInput: func(in *StartPhotoGradingInput) {
				in.ParentAutomaticDeadlineAt = 0
			},
			wantKind: "grading_request_invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
				Intent:         k12.ImageTaskIntentCompletedHomework,
				IntentEvidence: []string{"visible handwritten answers"},
				Confidence:     0.99,
			}}
			coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
			deps := *(coordinator.WorkFeedback.(*Deps))
			deps.Now = coordinator.Now
			deps.GradingBudgetSnapshot = tt.budget
			recognizer := &countingRecognizer{}
			delivery := &imageTaskPreflightDeliverySpy{}
			deps.Recognizer = recognizer
			deps.Delivery = delivery
			orchestrator := NewGradingOrchestrator(
				deps,
				func(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
					return k12.NormalizeGradingModelSnapshot(requested), nil
				},
			)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := orchestrator.Shutdown(ctx); err != nil {
					t.Errorf("Shutdown: %v", err)
				}
			})
			starter := &imageTaskPreflightFailureStarter{
				GradingOrchestrator: orchestrator,
				mutateInput:         tt.mutateInput,
			}
			coordinator.Grading = starter
			coordinator.GradingBudgetSnapshot = tt.budget

			view, created, runErr := createAndRunImageTask(
				t,
				coordinator,
				testCreateImageTaskInput(),
			)
			if !created || !errors.Is(runErr, ErrInvalidInput) {
				t.Fatalf("created=%v err=%v, want accepted dispatch and ErrInvalidInput", created, runErr)
			}
			if tt.wantIdentity != nil && !errors.Is(runErr, tt.wantIdentity) {
				t.Fatalf("err=%v, want typed identity %v", runErr, tt.wantIdentity)
			}
			if tt.wantIdentity == nil &&
				(errors.Is(runErr, errGradingBudgetMissing) ||
					errors.Is(runErr, errGradingBudgetPolicyInvalid) ||
					errors.Is(runErr, ErrModelRequestPolicyInvalid)) {
				t.Fatalf("generic invalid request carried a specific preflight identity: %v", runErr)
			}
			if starter.starts != 1 || starter.async != 0 {
				t.Fatalf("grading calls start=%d async=%d, want 1/0", starter.starts, starter.async)
			}
			if recognizer.calls != 0 || delivery.calls != 0 {
				t.Fatalf(
					"preflight failure crossed an external boundary: recognizer=%d delivery=%d",
					recognizer.calls,
					delivery.calls,
				)
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
				result.Dispatch.FailureKind != tt.wantKind ||
				!result.Dispatch.RetrySafe {
				t.Fatalf(
					"dispatch status=%q failure_kind=%q retry_safe=%v, want failed/%q/true",
					result.Dispatch.Status,
					result.Dispatch.FailureKind,
					result.Dispatch.RetrySafe,
					tt.wantKind,
				)
			}
			jobs, err := deps.ListGradingJobs(context.Background(), "mingming", "")
			if err != nil {
				t.Fatalf("ListGradingJobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("preflight failure created GradingJobs: %+v", jobs)
			}
		})
	}
}
