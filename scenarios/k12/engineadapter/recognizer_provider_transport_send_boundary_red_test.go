package engineadapter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/llm/openai"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type dd036ProviderRoundTripperFunc func(
	*http.Request,
) (*http.Response, error)

func (f dd036ProviderRoundTripperFunc) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	return f(req)
}

type dd036ProviderBoundaryHarness struct {
	store        *k12storage.Store
	orchestrator *usecase.GradingOrchestrator
	job          usecase.GradingJobView
}

func newDD036ProviderBoundaryHarness(
	t *testing.T,
	vision VisionFunc,
	sourceKey string,
) *dd036ProviderBoundaryHarness {
	t.Helper()

	store, constraint := newDD036CrossLayerStore(t)
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/gpt-5.6-sol",
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	orchestrator := usecase.NewGradingOrchestrator(
		usecase.Deps{
			Recognizer: NewRecognizerAdapter(
				vision,
				WithRecognizerProviderTransportSendBoundary(),
			),
			Records:    store,
			Constraint: constraint,
			Now:        func() int64 { return time.Now().Unix() },
		},
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := orchestrator.Shutdown(ctx); err != nil {
			t.Errorf("shutdown grading orchestrator: %v", err)
		}
	})
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		usecase.StartPhotoGradingInput{
			Photo: usecase.PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "dd036-provider-boundary",
				Image:         denseWorksheetTestImage(t, 1000, 1800),
			},
			SourceKind: "desktop",
			SourceKey:  sourceKey,
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	return &dd036ProviderBoundaryHarness{
		store:        store,
		orchestrator: orchestrator,
		job:          job,
	}
}

func dd036OpenAIProviderVision(
	provider *openai.Provider,
) VisionFunc {
	return func(
		ctx context.Context,
		image []byte,
		prompt string,
	) (string, error) {
		ctx = llm.WithOperationSafety(
			ctx,
			llm.OperationSafetyNonIdempotent,
		)
		dataURL := "data:image/png;base64," +
			base64.StdEncoding.EncodeToString(image)
		response, err := provider.Complete(ctx, llm.CompletionRequest{
			Model: k12.RecognizingPolicyModel,
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				MultiContent: []llm.ContentPart{
					llm.NewTextPart(prompt),
					llm.NewImageURLPart(dataURL, "high"),
				},
			}},
		})
		if err != nil {
			return "", err
		}
		if response == nil {
			return "", errors.New("provider returned a nil response")
		}
		return response.Content, nil
	}
}

func dd036RecognizingParent(
	t *testing.T,
	harness *dd036ProviderBoundaryHarness,
) k12.ModelInvocation {
	t.Helper()
	parents, err := harness.store.ListModelInvocations(
		context.Background(),
		"mingming",
		harness.job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list recognizing parents: %v", err)
	}
	for _, parent := range parents {
		if parent.Stage == k12.GradingStageRecognizing {
			return parent
		}
	}
	t.Fatalf("recognizing parent missing: %+v", parents)
	return k12.ModelInvocation{}
}

func dd036AssertProviderRequestNotSent(
	t *testing.T,
	harness *dd036ProviderBoundaryHarness,
	view usecase.GradingJobView,
) {
	t.Helper()
	if view.Record == nil ||
		(view.Record.Status != k12.GradingStageFailedRetryable &&
			view.Record.Status != k12.GradingStageFailedTerminal) {
		t.Fatalf(
			"provider request not sent job=%+v, want definite failed state",
			view,
		)
	}
	parent := dd036RecognizingParent(t, harness)
	if parent.Status != k12.ModelInvocationFailed {
		t.Fatalf(
			"provider request not sent parent=%+v, want definite failed",
			parent,
		)
	}
	children, err := harness.store.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		harness.job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list physical children: %v", err)
	}
	if len(children) != 1 ||
		children[0].Status != k12.ModelInvocationFailed ||
		children[0].FailureKind != "provider_request_not_sent" {
		t.Fatalf(
			"provider request not sent child=%+v, want one definite failed provider_request_not_sent receipt",
			children,
		)
	}
}

// REG-DD-036 RED: a sent stage parent is not evidence that a provider request
// escaped. The explicit RecognizerAdapter boundary must defer the physical
// child's prepared→sent CAS to ai-core's final transport hook. Any local
// sender failure, including ai-core request construction before Client.Do,
// therefore has HTTP=0 and converges child/parent/job to definite failure.
func TestDD036RecognizerProviderTransportPreflightIsDefinitelyNotSent(
	t *testing.T,
) {
	t.Run("provider_sender_local_preflight", func(t *testing.T) {
		var roundTrips atomic.Int32
		client := &http.Client{
			Transport: dd036ProviderRoundTripperFunc(
				func(*http.Request) (*http.Response, error) {
					roundTrips.Add(1)
					return nil, errors.New(
						"local provider preflight must prevent RoundTrip",
					)
				},
			),
		}
		provider := openai.New(
			"test-key",
			openai.WithBaseURL("https://provider.example/v1"),
			openai.WithHTTPClient(client),
			openai.WithModel(k12.RecognizingPolicyModel),
		)
		providerVision := dd036OpenAIProviderVision(provider)
		vision := func(
			ctx context.Context,
			image []byte,
			prompt string,
		) (string, error) {
			preflightErr := errors.New(
				"frozen provider lookup rejected before provider.Complete",
			)
			if preflightErr != nil {
				return "", preflightErr
			}
			return providerVision(ctx, image, prompt)
		}
		harness := newDD036ProviderBoundaryHarness(
			t,
			vision,
			"dd036-provider-local-preflight",
		)
		view, runErr := harness.orchestrator.RunGradingJob(
			context.Background(),
			harness.job.Record.RecordID,
		)
		if runErr == nil {
			t.Fatal("RunGradingJob error=nil, want local preflight failure")
		}
		if got := roundTrips.Load(); got != 0 {
			t.Fatalf("HTTP RoundTrips=%d, want 0", got)
		}
		dd036AssertProviderRequestNotSent(t, harness, view)
	})

	t.Run("ai_core_http_request_construction_preflight", func(t *testing.T) {
		var roundTrips atomic.Int32
		client := &http.Client{
			Transport: dd036ProviderRoundTripperFunc(
				func(*http.Request) (*http.Response, error) {
					roundTrips.Add(1)
					return nil, errors.New(
						"malformed URL must fail before RoundTrip",
					)
				},
			),
		}
		provider := openai.New(
			"test-key",
			openai.WithBaseURL("://invalid-provider-url"),
			openai.WithHTTPClient(client),
			openai.WithModel(k12.RecognizingPolicyModel),
		)
		harness := newDD036ProviderBoundaryHarness(
			t,
			dd036OpenAIProviderVision(provider),
			"dd036-ai-core-request-preflight",
		)
		view, runErr := harness.orchestrator.RunGradingJob(
			context.Background(),
			harness.job.Record.RecordID,
		)
		if runErr == nil {
			t.Fatal("RunGradingJob error=nil, want ai-core request preflight failure")
		}
		if got := roundTrips.Load(); got != 0 {
			t.Fatalf("HTTP RoundTrips=%d, want 0", got)
		}
		dd036AssertProviderRequestNotSent(t, harness, view)
	})
}

// REG-DD-036 RED: after every local preflight succeeds, ai-core's final
// before-send hook must win the durable child CAS before the HTTP client can
// observe the request. This is the actual provider transport boundary, not a
// composition-root callback or an adapter-local guess.
func TestDD036RecognizerProviderTransportObservesChildSentBeforeRoundTrip(
	t *testing.T,
) {
	var roundTrips atomic.Int32
	var jobID string
	var store *k12storage.Store
	var roundTripInvariant atomic.Value
	client := &http.Client{
		Transport: dd036ProviderRoundTripperFunc(
			func(req *http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				if req.Method != http.MethodPost {
					roundTripInvariant.Store(
						fmt.Sprintf("HTTP method=%s want POST", req.Method),
					)
				}
				children, err := store.ListModelPhysicalInvocations(
					context.Background(),
					"mingming",
					jobID,
				)
				if err != nil {
					roundTripInvariant.Store(
						fmt.Sprintf("list child at RoundTrip: %v", err),
					)
				} else if len(children) != 1 ||
					children[0].Status != k12.ModelInvocationSent {
					roundTripInvariant.Store(fmt.Sprintf(
						"RoundTrip observed children=%+v, want one CAS-sent child",
						children,
					))
				}
				body := fmt.Sprintf(
					`{"id":"dd036-http-request","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`,
					k12.RecognizingPolicyModel,
					dd036CrossLayerQuestionJSON,
				)
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			},
		),
	}
	provider := openai.New(
		"test-key",
		openai.WithBaseURL("https://provider.example/v1"),
		openai.WithHTTPClient(client),
		openai.WithModel(k12.RecognizingPolicyModel),
	)
	harness := newDD036ProviderBoundaryHarness(
		t,
		dd036OpenAIProviderVision(provider),
		"dd036-ai-core-transport-success",
	)
	store = harness.store
	jobID = harness.job.Record.RecordID

	view, runErr := harness.orchestrator.RunGradingJob(
		context.Background(),
		harness.job.Record.RecordID,
	)
	if runErr != nil {
		t.Fatalf("RunGradingJob success path: %v", runErr)
	}
	if got := roundTrips.Load(); got != 1 {
		t.Fatalf("HTTP RoundTrips=%d, want exactly 1", got)
	}
	if invariant := roundTripInvariant.Load(); invariant != nil {
		t.Fatal(invariant)
	}
	if view.Record == nil ||
		view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf(
			"successful provider boundary job=%+v, want awaiting confirmation",
			view,
		)
	}
	parent := dd036RecognizingParent(t, harness)
	if parent.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("successful recognizing parent=%+v", parent)
	}
	children, err := harness.store.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		harness.job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list successful physical child: %v", err)
	}
	if len(children) != 1 ||
		children[0].Status != k12.ModelInvocationSucceeded ||
		children[0].ResultDigest == "" {
		t.Fatalf(
			"successful physical child=%+v, want one durable success",
			children,
		)
	}
}
