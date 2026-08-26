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

func TestPublicImageTaskProjectsAuthoritativeRetryabilityAndStructuredFailureKind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stage     string
		retryable bool
	}{
		{name: "safe retry", stage: "failed_retryable", retryable: true},
		{name: "unknown is query only", stage: "outcome_unknown", retryable: false},
		{name: "terminal failure", stage: "failed_terminal", retryable: false},
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
