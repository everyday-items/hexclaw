package apihttp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func setRetryabilityForProjectionTest(t *testing.T, target any, value bool) {
	t.Helper()
	field := reflect.ValueOf(target).Elem().FieldByName("Retryable")
	if !field.IsValid() {
		t.Fatal("ImageTaskHomeworkProjection must expose public retry capability")
	}
	field.SetBool(value)
}

func setCreativeFeedbackRetryabilityForProjectionTest(t *testing.T, target any, value bool) {
	t.Helper()
	field := reflect.ValueOf(target).Elem().FieldByName("CreativeFeedbackRetryable")
	if !field.IsValid() {
		t.Fatal("ImageTaskView must expose the work-feedback invocation retry capability")
	}
	field.SetBool(value)
}

func TestPublicImageTaskProjectsAuthoritativeRetryabilityAndStructuredFailureKind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stage     string
		retryable bool
		wantState string
	}{
		{name: "safe retry", stage: "failed_retryable", retryable: true, wantState: "failed_retryable"},
		{name: "unknown is query only", stage: "outcome_unknown", retryable: false, wantState: "recovering"},
		{name: "terminal failure", stage: "failed_terminal", retryable: false, wantState: "failed_terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection := &usecase.ImageTaskHomeworkProjection{Stage: tc.stage}
			setRetryabilityForProjectionTest(t, projection, tc.retryable)
			view := usecase.ImageTaskView{
				Dispatch: k12.ImageTaskDispatch{
					DispatchID:  "dispatch-1",
					TaskIntent:  k12.ImageTaskIntentCompletedHomework,
					Status:      k12.ImageTaskStatusFailed,
					RetrySafe:   tc.retryable,
					FailureKind: "provider_response_http_502",
				},
				Homework:           &k12.HomeworkSubmission{},
				HomeworkProjection: projection,
			}

			wire := publicImageTask(view)
			field := reflect.ValueOf(wire).FieldByName("Retryable")
			if !field.IsValid() || field.Bool() != tc.retryable {
				t.Fatalf("public retryable=%v, want %v", field.IsValid() && field.Bool(), tc.retryable)
			}
			if wire.Progress.Operation != "homework" {
				t.Fatalf("public progress operation=%q, want homework", wire.Progress.Operation)
			}
			if wire.Progress.State != tc.wantState {
				t.Fatalf("public progress state=%q, want %q", wire.Progress.State, tc.wantState)
			}
			targetProjection, ok := wire.TargetProjection.(imageTaskHomeworkProjectionDTO)
			if !ok {
				t.Fatalf("public target projection type=%T, want imageTaskHomeworkProjectionDTO", wire.TargetProjection)
			}
			if targetProjection.Stage != tc.wantState {
				t.Fatalf("public target projection stage=%q, want %q", targetProjection.Stage, tc.wantState)
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if !strings.Contains(body, `"retryable":`) {
				t.Fatalf("public capability missing: %s", body)
			}
			if !strings.Contains(body, `"failure_kind":"provider_response_http_502"`) {
				t.Fatalf("public failed dispatch lost structured failure kind: %s", body)
			}
		})
	}
}

func TestPublicImageTaskOmitsFailureKindOutsideFailedStatus(t *testing.T) {
	wire := publicImageTask(usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
		DispatchID:  "dispatch-1",
		TaskIntent:  k12.ImageTaskIntentCompletedHomework,
		Status:      k12.ImageTaskStatusRouted,
		FailureKind: "stale_failure_must_not_leak",
	}})
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"failure_kind"`) {
		t.Fatalf("non-failed public dispatch leaked stale failure kind: %s", raw)
	}
}

func TestPublicImageTaskProjectsCreativeFeedbackInvocationRetryability(t *testing.T) {
	view := usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-artwork",
			TaskIntent: k12.ImageTaskIntentArtwork,
			Status:     k12.ImageTaskStatusRouted,
		},
		Creative: &k12.CreativeWorkIntake{
			WorkType:       k12.WorkTypeArt,
			Status:         k12.CreativeWorkIntakePromoted,
			PromotedWorkID: "work-artwork",
			RetrySafe:      false,
		},
		CreativeFeedback: "feedback_failed",
	}
	setCreativeFeedbackRetryabilityForProjectionTest(t, &view, false)

	wire := publicImageTask(view)
	if wire.Retryable {
		t.Fatal("public retryable=true, want failed work-feedback with retry_safe=false")
	}
	if wire.Progress.State != "feedback_failed" {
		t.Fatalf("public progress state=%q, want feedback_failed", wire.Progress.State)
	}
}
