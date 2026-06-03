package llmcall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
)

type recordingProvider struct {
	responses []*llm.CompletionResponse
	errors    []error
	calls     int
}

func (p *recordingProvider) Name() string                                  { return "rec" }
func (p *recordingProvider) Models() []llm.ModelInfo                       { return nil }
func (p *recordingProvider) CountTokens(_ []llm.Message) (int, error)      { return 0, nil }
func (p *recordingProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}
func (p *recordingProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	idx := p.calls
	p.calls++
	if idx < len(p.errors) && p.errors[idx] != nil {
		return nil, p.errors[idx]
	}
	if idx < len(p.responses) {
		return p.responses[idx], nil
	}
	return &llm.CompletionResponse{Content: "ok"}, nil
}

// TestCall_FirstAttemptSuccess
func TestCall_FirstAttemptSuccess(t *testing.T) {
	p := &recordingProvider{
		responses: []*llm.CompletionResponse{{Content: "hello"}},
	}
	var stages []ProgressStage
	resp, err := CallWithProgress(context.Background(), Request{
		Provider: p, ProviderName: "test",
		Req: llm.CompletionRequest{Model: "m"},
	}, func(prog Progress) { stages = append(stages, prog.Stage) })
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("content: %q", resp.Content)
	}
	if resp.Attempts != 1 {
		t.Errorf("attempts = %d", resp.Attempts)
	}
	want := []ProgressStage{StageStarting, StageCompleted}
	if !equalStages(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

// TestCall_TransientRetryThenSuccess
func TestCall_TransientRetryThenSuccess(t *testing.T) {
	p := &recordingProvider{
		errors: []error{errors.New("connection timeout"), errors.New("503 Service Unavailable"), nil},
		responses: []*llm.CompletionResponse{nil, nil, {Content: "third try"}},
	}
	var stages []ProgressStage
	resp, err := CallWithProgress(context.Background(), Request{
		Provider: p, ProviderName: "test",
		Req:          llm.CompletionRequest{Model: "m"},
		RetryBackoff: 1 * time.Millisecond,
	}, func(prog Progress) { stages = append(stages, prog.Stage) })
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Content != "third try" {
		t.Errorf("content: %q", resp.Content)
	}
	if resp.Attempts != 3 {
		t.Errorf("attempts = %d", resp.Attempts)
	}
	// stages: starting → retrying → retrying → completed
	if len(stages) != 4 {
		t.Errorf("stages: %v", stages)
	}
}

// TestCall_NonTransientFailsImmediately
func TestCall_NonTransientFailsImmediately(t *testing.T) {
	p := &recordingProvider{
		errors: []error{errors.New("model 'x' not found")},
	}
	_, err := CallWithProgress(context.Background(), Request{
		Provider:     p,
		Req:          llm.CompletionRequest{Model: "m"},
		MaxRetries:   5,
		RetryBackoff: 1 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if p.calls != 1 {
		t.Errorf("non-transient 应只调 1 次，实际 %d", p.calls)
	}
}

// TestCall_RetriesExhausted
func TestCall_RetriesExhausted(t *testing.T) {
	p := &recordingProvider{
		errors: []error{errors.New("timeout"), errors.New("timeout"), errors.New("timeout")},
	}
	_, err := CallWithProgress(context.Background(), Request{
		Provider: p, Req: llm.CompletionRequest{Model: "m"},
		MaxRetries: 3, RetryBackoff: 1 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("expected error after exhaust")
	}
	if !strings.Contains(err.Error(), "attempts=3") {
		t.Errorf("err: %v", err)
	}
}

// TestCall_NilProvider
func TestCall_NilProvider(t *testing.T) {
	_, err := CallWithProgress(context.Background(), Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider 为 nil") {
		t.Errorf("err: %v", err)
	}
}

// TestCall_ContextCancel
func TestCall_ContextCancel(t *testing.T) {
	p := &recordingProvider{
		errors: []error{errors.New("timeout"), errors.New("timeout"), errors.New("timeout")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CallWithProgress(ctx, Request{
		Provider: p, Req: llm.CompletionRequest{Model: "m"},
		MaxRetries: 5, RetryBackoff: 100 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("expected ctx.Err()")
	}
}

func equalStages(a, b []ProgressStage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
