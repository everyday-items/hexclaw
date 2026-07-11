package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

type semanticStatefulProbe struct{ calls atomic.Int32 }

func (s *semanticStatefulProbe) Name() string        { return "stateful_probe" }
func (s *semanticStatefulProbe) Description() string { return "mutates live state" }
func (s *semanticStatefulProbe) Match(string) bool   { return false }
func (s *semanticStatefulProbe) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.Name(), s.Description(), nil)
}
func (s *semanticStatefulProbe) Execute(context.Context, map[string]any) (*skill.Result, error) {
	n := s.calls.Add(1)
	return &skill.Result{Content: fmt.Sprintf("state-%d", n)}, nil
}

type semanticStatefulProvider struct{ turns atomic.Int32 }

func (p *semanticStatefulProvider) Name() string { return "test" }
func (p *semanticStatefulProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.CompletionResponse{Content: fmt.Sprintf("turn-%d", p.turns.Load())}, nil
		}
	}
	p.turns.Add(1)
	return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "stateful-call", Name: "stateful_probe", Arguments: "{}"}}}, nil
}
func (p *semanticStatefulProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, fmt.Errorf("stream unused")
}
func (p *semanticStatefulProvider) Models() []llm.ModelInfo                { return nil }
func (p *semanticStatefulProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func TestSemanticCache_StatefulToolResultIsNeverStored(t *testing.T) {
	probe := &semanticStatefulProbe{}
	reg := skill.NewRegistry()
	if err := reg.Register(probe); err != nil {
		t.Fatal(err)
	}
	provider := &semanticStatefulProvider{}
	eng := newEngineWithProviderAndSkills(t, provider, reg)
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))

	for i := 0; i < 2; i++ {
		msg := &adapter.Message{
			ID:       fmt.Sprintf("stateful-%d", i),
			Platform: adapter.PlatformAPI,
			UserID:   "user-stateful",
			ChatID:   "chat-stateful",
			Content:  "执行一次自定义状态动作",
		}
		reply, err := eng.Process(context.Background(), msg)
		if err != nil {
			t.Fatalf("Process[%d]: %v", i, err)
		}
		if reply.Metadata["source"] == "cache" {
			t.Fatalf("Process[%d] served stateful result from semantic cache", i)
		}
	}
	if got := probe.calls.Load(); got != 2 {
		t.Fatalf("stateful tool must execute once per identical request, got %d", got)
	}
	if got := provider.turns.Load(); got != 2 {
		t.Fatalf("provider initial turns = %d, want 2 (second request must miss cache)", got)
	}
}

var _ hexagon.Provider = (*semanticStatefulProvider)(nil)
