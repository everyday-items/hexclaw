package apihttp_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type imageTaskHTTPRetryableGradingStub struct {
	jobID             string
	genericRetryCalls int
	parentRetryCalls  int
}

func (s *imageTaskHTTPRetryableGradingStub) StartPhotoGradingJob(
	_ context.Context,
	_ usecase.StartPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{Record: &records.AgentRecord{RecordID: s.jobID}}, true, nil
}

func (*imageTaskHTTPRetryableGradingStub) ConfirmPhotoGradingJob(
	context.Context,
	string,
	usecase.ConfirmPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{}, false, nil
}

func (*imageTaskHTTPRetryableGradingStub) StartAsync(string) bool { return true }

func (*imageTaskHTTPRetryableGradingStub) CanRetryPhotoGradingWithParentAutomaticWindow(
	context.Context,
	string,
) (bool, error) {
	return false, nil
}

func (s *imageTaskHTTPRetryableGradingStub) RetryPhotoGradingJobWithParentAutomaticWindow(
	context.Context,
	string,
	string,
	int64,
) (usecase.GradingJobView, bool, error) {
	s.parentRetryCalls++
	return usecase.GradingJobView{}, false, nil
}

func (*imageTaskHTTPRetryableGradingStub) ImageTaskHomeworkProjection(
	context.Context,
	string,
	string,
) (usecase.ImageTaskHomeworkProjection, error) {
	return usecase.ImageTaskHomeworkProjection{
		Stage:     k12.GradingStageFailedRetryable,
		Retryable: true,
	}, nil
}

func (s *imageTaskHTTPRetryableGradingStub) RetryPhotoGradingJob(
	_ context.Context,
	jobID string,
) (usecase.GradingJobView, bool, error) {
	if jobID != s.jobID {
		return usecase.GradingJobView{}, false, fmt.Errorf("unexpected job id %q", jobID)
	}
	s.genericRetryCalls++
	return usecase.GradingJobView{Record: &records.AgentRecord{RecordID: jobID}}, true, nil
}

// K12-IMAGE-TASK-RETRYABLE-RETRY-001: the only public retry endpoint must
// accept a definite failed_retryable child Job, reuse the same dispatch, and
// invoke the shared generic path without exposing an internal GradingJob route.
func TestBUG20260802PublicImageTaskRetryUsesGenericRetryableGradingPath(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.classifier.result = usecase.ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}
	grading := &imageTaskHTTPRetryableGradingStub{jobID: "http-retryable-job"}
	fixture.coordinator.Grading = grading
	created, fresh, err := fixture.coordinator.Create(context.Background(), usecase.CreateImageTaskInput{
		AgentName:         "mingming",
		LearnerID:         "learner-retryable-http",
		SourceKind:        k12.ImageTaskSourceDesktop,
		SourceRef:         "message-retryable-http",
		SourceSessionID:   "session-retryable-http",
		SourceAssetRefs:   []string{fixture.assetID},
		MessageIntent:     "请批改",
		AttemptGeneration: 1,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
		},
	})
	if err != nil || !fresh {
		t.Fatalf("create image task: fresh=%v err=%v", fresh, err)
	}
	view, err := fixture.coordinator.Run(context.Background(), "mingming", created.Dispatch.DispatchID)
	if err != nil {
		t.Fatalf("route image task: %v", err)
	}

	rec, out := do(t, fixture.handler, http.MethodPost,
		"/image-tasks/"+view.Dispatch.DispatchID+"/retry",
		fmt.Sprintf(`{"agent":"mingming","version":%d}`, view.Dispatch.Version),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("public image-task retry status=%d body=%s", rec.Code, rec.Body.String())
	}
	dispatch, ok := out["dispatch"].(map[string]any)
	if !ok || dispatch["dispatch_id"] != view.Dispatch.DispatchID {
		t.Fatalf("retry must keep the same public dispatch: %#v", out)
	}
	if grading.genericRetryCalls != 1 || grading.parentRetryCalls != 0 {
		t.Fatalf(
			"retry route used wrong child retry path: generic=%d parent=%d",
			grading.genericRetryCalls,
			grading.parentRetryCalls,
		)
	}
}
