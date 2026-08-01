package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func dd036SendBoundarySnapshot() k12.GradingModelSnapshot {
	policy := k12.ApprovedRecognizingRequestPolicy()
	return k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: policy,
	}
}

type dd036NotSentRecognizer struct {
	preflightCalls int
	httpPOSTs      int
}

func (r *dd036NotSentRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	_, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			r.preflightCalls++
			rejectedBeforePOST := true
			if rejectedBeforePOST {
				return "", fmt.Errorf(
					"%w: frozen provider lookup rejected before HTTP POST",
					k12.ErrRecognitionPhysicalCallBeforeSend,
				)
			}
			r.httpPOSTs++
			return `[{"question":"must not be reached"}]`, nil
		},
	)
	return nil, err
}

// REG-DD-036 RED: a durable child claim is authorization to send, not proof
// that an HTTP POST happened. A typed not-sent result from provider lookup,
// capability, egress, request-build, or rate-limit preflight is definitive and
// must not strand either the physical child or its stage parent as unknown.
func TestDD036PhysicalNotSentMarkerIsDefinitiveForChildAndParent(t *testing.T) {
	recognizer := &dd036NotSentRecognizer{}
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
	deps.Recognizer = recognizer
	snapshot := dd036SendBoundarySnapshot()
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  "dd036-provider-preflight-not-sent",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}

	_, runErr := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	)
	if !errors.Is(runErr, k12.ErrRecognitionPhysicalCallBeforeSend) {
		t.Fatalf("run error=%v, want typed not-sent marker", runErr)
	}
	if recognizer.preflightCalls != 1 || recognizer.httpPOSTs != 0 {
		t.Fatalf(
			"provider boundary preflight=%d HTTP POSTs=%d, want 1/0",
			recognizer.preflightCalls,
			recognizer.httpPOSTs,
		)
	}

	parents, err := deps.Records.ListModelInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 || parents[0].Status != k12.ModelInvocationFailed {
		t.Fatalf("not-sent parent must be definitive failed: %+v", parents)
	}
	if len(children) != 1 ||
		children[0].Status != k12.ModelInvocationFailed ||
		children[0].FailureKind == "" {
		t.Fatalf(
			"zero-POST physical child must be definitive failed, never outcome_unknown: %+v",
			children,
		)
	}
}

func newDD036PhysicalExecutorHarness(
	t *testing.T,
	sourceKey string,
) (
	*durableRecognitionPhysicalCallExecutor,
	Deps,
	GradingJobView,
) {
	t.Helper()
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
	snapshot := dd036SendBoundarySnapshot()
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  sourceKey,
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	parent, parentCreated, err := deps.Records.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:          "parent-" + sourceKey,
			AgentName:             job.Record.AgentName,
			JobID:                 job.Record.RecordID,
			Stage:                 k12.GradingStageRecognizing,
			RequestDigest:         "sha256:" + sourceKey,
			RouteSnapshot:         snapshot,
			RequestPolicySnapshot: snapshot.RecognizingRequestPolicy,
			Attempt:               1,
			CreatedAt:             deps.now(),
			UpdatedAt:             deps.now(),
		},
	)
	if err != nil || !parentCreated {
		t.Fatalf("prepare parent created=%v err=%v", parentCreated, err)
	}
	parent, err = deps.Records.MarkModelInvocationSent(
		context.Background(),
		parent.AgentName,
		parent.InvocationID,
		"",
	)
	if err != nil {
		t.Fatalf("mark parent sent: %v", err)
	}
	return newDurableRecognitionPhysicalCallExecutor(
		orchestrator,
		parent,
	), deps, job
}

// REG-DD-036 RED: cancellation/deadline is not allowed to erase a definitive
// HTTP response that arrived for the same physical attempt. Late HTTP 200
// remains fenced by the existing regression; typed 4xx/5xx is a known failed
// result and must win over the concurrently-observed context error.
func TestDD036PhysicalDefinitiveHTTPResponseWinsConcurrentCancellation(
	t *testing.T,
) {
	for _, status := range []int{400, 500} {
		t.Run(fmt.Sprintf("http_%d", status), func(t *testing.T) {
			executor, deps, job := newDD036PhysicalExecutorHarness(
				t,
				fmt.Sprintf("dd036-definitive-after-cancel-%d", status),
			)
			callCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, callErr := executor.ExecuteRecognitionPhysicalCall(
				callCtx,
				k12.RecognitionPhysicalCall{
					Unit: k12.RecognitionPhysicalUnitWholePage,
					Image: []byte(
						fmt.Sprintf("definitive-http-%d-image", status),
					),
				},
				func(providerCtx context.Context) (string, error) {
					cancel()
					<-providerCtx.Done()
					return "", &gradingProviderResponseError{status: status}
				},
			)
			var providerErr *gradingProviderResponseError
			if !errors.As(callErr, &providerErr) || providerErr.status != status {
				t.Fatalf(
					"call error=%v, want definitive HTTP %d preserved",
					callErr,
					status,
				)
			}

			children, err := deps.Records.ListModelPhysicalInvocations(
				context.Background(),
				job.Record.AgentName,
				job.Record.RecordID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 1 ||
				children[0].Status != k12.ModelInvocationFailed ||
				children[0].FailureKind !=
					fmt.Sprintf("provider_response_http_%d", status) {
				t.Fatalf(
					"definitive HTTP %d must win over cancellation: %+v",
					status,
					children,
				)
			}
		})
	}
}
