package llmrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

type localInferenceProviderDouble struct {
	name          string
	completeCalls atomic.Int32
	streamCalls   atomic.Int32
	complete      func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error)
	stream        func(context.Context, llm.CompletionRequest) (*llm.Stream, error)
}

func (p *localInferenceProviderDouble) Name() string {
	if p.name != "" {
		return p.name
	}
	return "ollama"
}
func (p *localInferenceProviderDouble) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.completeCalls.Add(1)
	if p.complete == nil {
		return &llm.CompletionResponse{}, nil
	}
	return p.complete(ctx, req)
}
func (p *localInferenceProviderDouble) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.streamCalls.Add(1)
	if p.stream == nil {
		return nil, nil
	}
	return p.stream(ctx, req)
}
func (p *localInferenceProviderDouble) Models() []llm.ModelInfo                { return nil }
func (p *localInferenceProviderDouble) CountTokens([]llm.Message) (int, error) { return 0, nil }

func newLocalInferenceProviderTestCoordinator(t *testing.T) (*localinfer.Coordinator, *resourcegov.Governor) {
	t.Helper()
	governor, err := resourcegov.New(resourcegov.Config{
		Limits: map[resourcegov.Resource]int{
			resourcegov.ResourceVLM:            1,
			resourcegov.ResourceLocalInference: 1,
			resourcegov.ResourceCPUHeavy:       1,
			resourcegov.ResourceSQLiteWrite:    1,
		},
		BackgroundAging: time.Second, MaxInteractiveBurst: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	return localinfer.New(governor), governor
}

func waitLocalInferenceMetric(t *testing.T, governor *resourcegov.Governor, predicate func(resourcegov.ResourceMetrics) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
		if predicate(metric) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("local inference metric did not converge: %+v", governor.Snapshot().Resources[resourcegov.ResourceLocalInference])
}

func TestLocalInferenceProviderCompleteSharesCoordinatorAndHonorsParentDeadline(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	_, hold, err := coordinator.Acquire(context.Background(), localinfer.OperationQueryEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	invoked := make(chan time.Duration, 1)
	next := &localInferenceProviderDouble{complete: func(ctx context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("local qwen call must have a model-scoped ceiling")
		}
		invoked <- time.Until(deadline)
		return &llm.CompletionResponse{Content: "ok"}, nil
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: localChatBudget,
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, callErr := provider.Complete(parent, llm.CompletionRequest{})
		done <- callErr
	}()
	waitLocalInferenceMetric(t, governor, func(metric resourcegov.ResourceMetrics) bool {
		return metric.QueuedByPriority[resourcegov.PriorityInteractive] == 1
	})
	hold.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	remaining := <-invoked
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("child deadline remaining=%v, want shorter parent deadline", remaining)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("complete leaked permit: in_use=%d", got)
	}
}

func TestLocalInferenceProviderStreamHoldsPermitUntilClose(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	next := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
		return streamx.NewStream(strings.NewReader("data: [DONE]\n"), streamx.OpenAIFormat), nil
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: localChatBudget,
	}
	stream, err := provider.Stream(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 1 {
		t.Fatalf("permit released when Stream returned: in_use=%d", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	waitLocalInferenceMetric(t, governor, func(metric resourcegov.ResourceMetrics) bool { return metric.InUse == 0 })
	if got := coordinator.Snapshot().Operations[localinfer.OperationChat]; got.Completed != 1 {
		t.Fatalf("chat metrics=%+v", got)
	}
}

func TestLocalInferenceProviderStreamDeadlineClosesUnconsumedStream(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	next := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
		return streamx.NewStream(strings.NewReader("data: never-consumed\n"), streamx.OpenAIFormat), nil
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: func(string) time.Duration { return 30 * time.Millisecond },
	}
	stream, err := provider.Stream(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("model deadline did not close an unconsumed stream")
	}
	waitLocalInferenceMetric(t, governor, func(metric resourcegov.ResourceMetrics) bool { return metric.InUse == 0 })
}

func TestLocalInferenceProviderPreservesLazyCallbackRegistration(t *testing.T) {
	coordinator, _ := newLocalInferenceProviderTestCoordinator(t)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	next := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
		return streamx.NewStream(reader, streamx.OpenAIFormat), nil
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: func(string) time.Duration { return time.Second },
	}
	stream, err := provider.Stream(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	doneCallback := make(chan struct{}, 1)
	stream.OnDone(func(*llm.StreamResult) { doneCallback <- struct{}{} })
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(writer, "data: [DONE]\n")
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()
	for range stream.Chunks() {
	}
	<-stream.Done()
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-doneCallback:
	case <-time.After(time.Second):
		t.Fatal("provider wrapper started stream before caller registered OnDone")
	}
}

func TestLocalInferenceProviderStreamPanicReleasesPermit(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	next := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
		panic("stream provider panic canary")
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: localChatBudget,
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("provider panic was not propagated")
			}
		}()
		_, _ = provider.Stream(context.Background(), llm.CompletionRequest{})
	}()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("stream panic leaked permit: in_use=%d", got)
	}
}

func TestLocalInferenceProviderMalformedStreamRecordsFailure(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	next := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
		return streamx.NewStream(strings.NewReader("data: definitely-not-json\n"), streamx.OpenAIFormat), nil
	}}
	provider := &localInferenceProvider{
		next: next, coordinator: coordinator, defaultModel: "qwen3.5:9b",
		budgetForModel: localChatBudget,
	}
	stream, err := provider.Stream(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Chunks() {
	}
	<-stream.Done()
	waitLocalInferenceMetric(t, governor, func(metric resourcegov.ResourceMetrics) bool {
		return metric.InUse == 0
	})
	metric := coordinator.Snapshot().Operations[localinfer.OperationChat]
	wantFailed := uint64(0)
	if _, preciseErrors := any(stream).(localInferenceStreamError); preciseErrors {
		wantFailed = 1
	}
	if metric.Attempts != 1 || metric.Admitted != 1 || metric.Completed != 1 || metric.Failed != wantFailed {
		t.Fatalf("malformed stream terminal metrics=%+v", metric)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("malformed stream leaked permit: in_use=%d", got)
	}
}

func TestLocalChatBudgetIsExactModelScoped(t *testing.T) {
	for _, test := range []struct {
		model string
		want  time.Duration
	}{
		{model: "qwen3.5:9b", want: 360 * time.Second},
		{model: " QWEN3.5:9B ", want: 360 * time.Second},
		{model: "qwen3.5:14b", want: 0},
		{model: "qwen3:9b", want: 0},
		{model: "gpt-5.6-sol", want: 0},
	} {
		if got := localChatBudget(test.model); got != test.want {
			t.Errorf("localChatBudget(%q)=%v, want %v", test.model, got, test.want)
		}
	}
}

func TestSelectorCloudProviderBypassesLocalInferenceCoordinator(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	const model = "gpt-5.6-sol"
	inner := &localInferenceProviderDouble{
		name: "openai",
		complete: func(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
			return &llm.CompletionResponse{Content: "ok"}, nil
		},
	}
	cfg := config.LLMConfig{
		Default: "openai",
		Providers: map[string]config.LLMProviderConfig{
			"openai": textModelProviderConfig(model, config.ProviderLocalityCloud),
		},
	}
	selector := NewWithProviders(cfg, map[string]hexagon.Provider{"openai": inner})
	selector.SetLocalInferenceCoordinator(coordinator)

	provider, ok := selector.Get("openai")
	if !ok {
		t.Fatal("cloud provider missing")
	}
	response, err := provider.Complete(context.Background(), llm.CompletionRequest{Model: model})
	if err != nil {
		t.Fatalf("cloud Complete: %v", err)
	}
	if response == nil || response.Content != "ok" {
		t.Fatalf("cloud response=%#v, want content ok", response)
	}
	if got := inner.completeCalls.Load(); got != 1 {
		t.Fatalf("cloud transport calls=%d, want 1", got)
	}
	if got := coordinator.Snapshot().Operations[localinfer.OperationChat]; got.Attempts != 0 {
		t.Fatalf("cloud call entered local coordinator: %+v", got)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("cloud call consumed local permit: in_use=%d", got)
	}
}

func TestSelectorReloadPreservesLocalInferenceCoordinator(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests <- request.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.5:9b","done":true,"message":{"role":"assistant","content":"ok"}}`))
	}))
	t.Cleanup(server.Close)

	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	const (
		providerName = "ollama"
		model        = "qwen3.5:9b"
	)
	initialCfg := config.LLMConfig{
		Default: providerName,
		Providers: map[string]config.LLMProviderConfig{
			providerName: textModelProviderConfig(model, config.ProviderLocalityLocal),
		},
	}
	selector := NewWithProviders(initialCfg, map[string]hexagon.Provider{
		providerName: &localInferenceProviderDouble{},
	})
	selector.SetLocalInferenceCoordinator(coordinator)

	reloadedCfg := initialCfg
	reloadedProvider := textModelProviderConfig(model, config.ProviderLocalityLocal)
	reloadedProvider.BaseURL = server.URL + "/v1"
	reloadedCfg.Providers = map[string]config.LLMProviderConfig{providerName: reloadedProvider}
	if err := selector.Reload(reloadedCfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	provider := selector.Default()
	if provider == nil {
		t.Fatal("reloaded default provider is nil")
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := provider.Complete(callCtx, llm.CompletionRequest{Model: model})
	if err != nil {
		t.Fatalf("reloaded local Complete: %v", err)
	}
	if response == nil || response.Content != "ok" {
		t.Fatalf("reloaded response=%#v, want content ok", response)
	}
	select {
	case path := <-requests:
		if path != "/api/chat" {
			t.Fatalf("reloaded provider path=%q, want /api/chat", path)
		}
	case <-time.After(time.Second):
		t.Fatal("reloaded provider did not call deterministic Ollama stub")
	}
	metric := coordinator.Snapshot().Operations[localinfer.OperationChat]
	if metric.Attempts != 1 || metric.Admitted != 1 || metric.Completed != 1 {
		t.Fatalf("coordinator was not retained across Reload: %+v", metric)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("reloaded local call leaked permit: in_use=%d", got)
	}
}

func TestSelectorCapabilityFailureDoesNotAcquireLocalInferencePermit(t *testing.T) {
	coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
	const (
		providerName = "ollama"
		vectorModel  = "qwen3-embedding:0.6b"
	)
	providerConfig := config.LLMProviderConfig{
		Model:          vectorModel,
		Models:         []string{vectorModel},
		ModelSpecsMode: config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: vectorModel, Capabilities: []string{config.LLMModelCapabilityEmbedding},
		}},
		Locality: config.ProviderLocalityLocal,
	}
	inner := &localInferenceProviderDouble{}
	selector := NewWithProviders(config.LLMConfig{
		Default: providerName,
		Providers: map[string]config.LLMProviderConfig{
			providerName: providerConfig,
		},
	}, map[string]hexagon.Provider{providerName: inner})
	selector.SetLocalInferenceCoordinator(coordinator)
	provider, ok := selector.Get(providerName)
	if !ok {
		t.Fatal("local provider missing")
	}

	if _, err := provider.Complete(context.Background(), llm.CompletionRequest{Model: vectorModel}); !errors.Is(err, ErrModelCapabilityMismatch) {
		t.Fatalf("Complete error=%v, want ErrModelCapabilityMismatch", err)
	}
	if _, err := provider.Stream(context.Background(), llm.CompletionRequest{Model: vectorModel}); !errors.Is(err, ErrModelCapabilityMismatch) {
		t.Fatalf("Stream error=%v, want ErrModelCapabilityMismatch", err)
	}
	if got := inner.completeCalls.Load(); got != 0 {
		t.Fatalf("invalid Complete reached transport %d times", got)
	}
	if got := inner.streamCalls.Load(); got != 0 {
		t.Fatalf("invalid Stream reached transport %d times", got)
	}
	if got := coordinator.Snapshot().Operations[localinfer.OperationChat]; got.Attempts != 0 {
		t.Fatalf("capability rejection acquired local coordinator: %+v", got)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("capability rejection consumed local permit: in_use=%d", got)
	}
}

func TestLocalInferenceProviderStreamTerminalReleasesPermitExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name                   string
		terminate              func(*llm.Stream, context.CancelFunc) error
		wantLifecycleCancelled uint64
		wantLegacyCancelled    uint64
		naturalEOF             bool
	}{
		{
			name: "EOF",
			terminate: func(stream *llm.Stream, _ context.CancelFunc) error {
				for range stream.Chunks() {
				}
				return nil
			},
			naturalEOF: true,
		},
		{
			name: "Close",
			terminate: func(stream *llm.Stream, _ context.CancelFunc) error {
				return stream.Close()
			},
			wantLifecycleCancelled: 1,
			wantLegacyCancelled:    1,
		},
		{
			name: "cancel",
			terminate: func(_ *llm.Stream, cancel context.CancelFunc) error {
				cancel()
				return nil
			},
			wantLifecycleCancelled: 1,
			wantLegacyCancelled:    1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, governor := newLocalInferenceProviderTestCoordinator(t)
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = writer.Close() })
			inner := &localInferenceProviderDouble{stream: func(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
				return streamx.NewStream(reader, streamx.OpenAIFormat), nil
			}}
			provider := &localInferenceProvider{
				next: inner, coordinator: coordinator, defaultModel: "qwen3.5:9b",
				budgetForModel: func(string) time.Duration { return 50 * time.Millisecond },
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := provider.Stream(ctx, llm.CompletionRequest{})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 1 {
				t.Fatalf("permit not held before %s: in_use=%d", test.name, got)
			}
			var eofWrite <-chan error
			if test.naturalEOF {
				writeDone := make(chan error, 1)
				eofWrite = writeDone
				go func() {
					_, writeErr := io.WriteString(writer, "data: [DONE]\n")
					if closeErr := writer.Close(); writeErr == nil {
						writeErr = closeErr
					}
					writeDone <- writeErr
				}()
			}
			if err := test.terminate(stream, cancel); err != nil {
				t.Fatalf("terminate %s: %v", test.name, err)
			}
			if eofWrite != nil {
				if err := <-eofWrite; err != nil {
					t.Fatalf("write EOF fixture: %v", err)
				}
			}
			_, preciseLifecycle := any(stream).(localInferenceStreamLifecycle)
			// ai-core v0.2.5 cannot publish Done for Close-before-Start. The
			// provider still releases at its absolute deadline without stealing
			// the caller's callback-registration window. Newer ai-core publishes
			// the terminal state and releases immediately.
			if preciseLifecycle || test.name != "Close" {
				select {
				case <-stream.Done():
				case <-time.After(time.Second):
					t.Fatalf("stream did not terminate after %s", test.name)
				}
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("first post-terminal Close: %v", err)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("second post-terminal Close: %v", err)
			}
			waitLocalInferenceMetric(t, governor, func(metric resourcegov.ResourceMetrics) bool {
				return metric.InUse == 0
			})

			metric := coordinator.Snapshot().Operations[localinfer.OperationChat]
			if metric.Attempts != 1 || metric.Admitted != 1 || metric.Completed != 1 {
				t.Fatalf("%s release count drifted: %+v", test.name, metric)
			}
			wantCancelled := test.wantLegacyCancelled
			if preciseLifecycle {
				wantCancelled = test.wantLifecycleCancelled
			}
			if metric.Cancelled != wantCancelled {
				t.Fatalf("%s cancelled=%d, want %d", test.name, metric.Cancelled, wantCancelled)
			}
			if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
				t.Fatalf("%s leaked permit: in_use=%d", test.name, got)
			}
		})
	}
}

func textModelProviderConfig(model, locality string) config.LLMProviderConfig {
	return config.LLMProviderConfig{
		Model: model, Models: []string{model},
		ModelSpecsMode: config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: model, Capabilities: []string{config.LLMModelCapabilityText},
		}},
		Locality: locality,
	}
}
