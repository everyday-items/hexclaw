package llmrouter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/trace"
)

func TestCapabilityProbeRedactsNoToolResponseBeforePersistence(t *testing.T) {
	const responseSecret = "probe-response-must-not-be-persisted"
	provider := &fakeProvider{
		name:     "mock",
		response: &llm.CompletionResponse{Content: responseSecret},
	}
	store := newMemStore()
	service := NewCapabilityService(&Selector{providers: map[string]hexagon.Provider{"mock": provider}}, store)

	capability, err := service.Probe(context.Background(), "mock", "mock-model")
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	assertProbeErrorDoesNotContain(t, capability.ProbeError, responseSecret)
	want := "tool_call_absent response_len=" + strconv.Itoa(len(responseSecret))
	if capability.ProbeError != want {
		t.Fatalf("ProbeError = %q, want %q", capability.ProbeError, want)
	}
	persisted, err := store.GetCapability(context.Background(), "mock", "mock-model")
	if err != nil || persisted == nil {
		t.Fatalf("persisted capability = %#v, err=%v", persisted, err)
	}
	assertProbeErrorDoesNotContain(t, persisted.ProbeError, responseSecret)
}

func TestCapabilityProbeRedactsProviderErrorBodyBeforePersistence(t *testing.T) {
	const rawBody = `{"error":{"message":"provider-body-must-not-be-persisted"}}`
	provider := &fakeProvider{
		name: "mock",
		completeErr: &llm.ProviderError{
			StatusCode: 429,
			Status:     "429 Too Many Requests",
			Body:       rawBody,
		},
	}
	store := newMemStore()
	service := NewCapabilityService(&Selector{providers: map[string]hexagon.Provider{"mock": provider}}, store)

	capability, err := service.Probe(context.Background(), "mock", "mock-model")
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	assertProbeErrorDoesNotContain(t, capability.ProbeError, "provider-body-must-not-be-persisted")
	want := "complete_rate_limit body_len=" + strconv.Itoa(len(rawBody))
	if capability.ProbeError != want {
		t.Fatalf("ProbeError = %q, want %q", capability.ProbeError, want)
	}
	persisted, err := store.GetCapability(context.Background(), "mock", "mock-model")
	if err != nil || persisted == nil {
		t.Fatalf("persisted capability = %#v, err=%v", persisted, err)
	}
	assertProbeErrorDoesNotContain(t, persisted.ProbeError, "provider-body-must-not-be-persisted")
}

func TestCapabilityProbeRedactsToolArgumentValueBeforePersistence(t *testing.T) {
	const argumentSecret = "tool-argument-must-not-be-persisted"
	provider := &fakeProvider{
		name: "mock",
		response: &llm.CompletionResponse{ToolCalls: []llm.ToolCall{
			{Name: "echo", Arguments: `{"text":"` + argumentSecret + `"}`},
		}},
	}
	store := newMemStore()
	service := NewCapabilityService(&Selector{providers: map[string]hexagon.Provider{"mock": provider}}, store)

	capability, err := service.Probe(context.Background(), "mock", "mock-model")
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	assertProbeErrorDoesNotContain(t, capability.ProbeError, argumentSecret)
	want := "argument_text_mismatch text_len=" + strconv.Itoa(len(argumentSecret))
	if capability.ProbeError != want {
		t.Fatalf("ProbeError = %q, want %q", capability.ProbeError, want)
	}
	persisted, err := store.GetCapability(context.Background(), "mock", "mock-model")
	if err != nil || persisted == nil {
		t.Fatalf("persisted capability = %#v, err=%v", persisted, err)
	}
	assertProbeErrorDoesNotContain(t, persisted.ProbeError, argumentSecret)
}

func TestCapabilityProbeUpsertFailureLogOmitsRawError(t *testing.T) {
	const errorSecret = "capability-store-error-must-not-be-logged"
	provider := &fakeProvider{
		name: "mock",
		response: &llm.CompletionResponse{ToolCalls: []llm.ToolCall{
			{Name: "echo", Arguments: `{"text":"hello"}`},
		}},
	}
	var output bytes.Buffer
	ctx := trace.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&output, nil)))
	service := NewCapabilityService(
		&Selector{providers: map[string]hexagon.Provider{"mock": provider}},
		capabilityStoreFailure{err: errors.New(errorSecret)},
	)

	if _, err := service.Probe(ctx, "mock", "mock-model"); err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	logged := output.String()
	if strings.Contains(logged, errorSecret) {
		t.Fatalf("upsert failure log retained raw error: %s", logged)
	}
	if !strings.Contains(logged, `"err_class":"capability_store_error"`) || !strings.Contains(logged, `"err_len":`) {
		t.Fatalf("upsert failure log omitted safe diagnostics: %s", logged)
	}
}

type capabilityStoreFailure struct {
	err error
}

func (s capabilityStoreFailure) GetCapability(context.Context, string, string) (*Capability, error) {
	return nil, nil
}

func (s capabilityStoreFailure) UpsertCapability(context.Context, Capability) error {
	return s.err
}

func (capabilityStoreFailure) ListCapabilities(context.Context) ([]Capability, error) {
	return nil, nil
}

func assertProbeErrorDoesNotContain(t *testing.T, probeError string, secret string) {
	t.Helper()
	if strings.Contains(probeError, secret) {
		t.Fatalf("ProbeError retained model/provider payload %q: %q", secret, probeError)
	}
}
