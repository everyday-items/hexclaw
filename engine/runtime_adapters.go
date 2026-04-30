package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/trace"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

type runtimeProviderSelector struct {
	router interface {
		Fallback(exclude ...string) (hexagon.Provider, string, error)
	}
	initialProvider  hexagon.Provider
	initialName      string
	initialModel     string
	explicitProvider bool
	modelForProvider func(string) string
	wrapProvider     func(hexagon.Provider, string, string) hexagon.Provider
	currentProvider  hexagon.Provider
	currentName      string
	currentModel     string
}

func (s *runtimeProviderSelector) Select(context.Context, hruntime.Request) (hruntime.ProviderSelection, error) {
	if s.initialProvider == nil {
		return hruntime.ProviderSelection{}, hruntime.ErrNoProvider
	}
	provider := s.wrap(s.initialProvider, s.initialName, s.initialModel)
	s.currentProvider = provider
	s.currentName = s.initialName
	s.currentModel = s.initialModel
	return hruntime.ProviderSelection{
		Provider: provider,
		Name:     s.initialName,
		Model:    s.initialModel,
	}, nil
}

func (s *runtimeProviderSelector) Fallback(_ context.Context, failed hruntime.ProviderSelection, _ error) (hruntime.ProviderSelection, error) {
	if s.explicitProvider || s.router == nil {
		return hruntime.ProviderSelection{}, hruntime.ErrNoFallback
	}
	p, name, err := s.router.Fallback(failed.Name)
	if err != nil {
		return hruntime.ProviderSelection{}, err
	}
	model := name
	if s.modelForProvider != nil {
		model = s.modelForProvider(name)
	}
	p = s.wrap(p, name, model)
	s.currentProvider = p
	s.currentName = name
	s.currentModel = model
	return hruntime.ProviderSelection{Provider: p, Name: name, Model: model}, nil
}

func (s *runtimeProviderSelector) wrap(provider hexagon.Provider, name, model string) hexagon.Provider {
	if s.wrapProvider == nil || provider == nil {
		return provider
	}
	return s.wrapProvider(provider, name, model)
}

func (s *runtimeProviderSelector) Current() (hexagon.Provider, string, string) {
	if s.currentProvider == nil {
		return s.initialProvider, s.initialName, s.initialModel
	}
	return s.currentProvider, s.currentName, s.currentModel
}

type thinkingRecoveryTracker struct {
	timeout   atomic.Bool
	recovered atomic.Bool
}

type thinkingBoundProvider struct {
	engine       *ReActEngine
	provider     hexagon.Provider
	providerName string
	modelName    string
	tracker      *thinkingRecoveryTracker
}

func (p *thinkingBoundProvider) Name() string { return p.provider.Name() }

func (p *thinkingBoundProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	resp, thinkingTimedOut, err := p.engine.completeWithThinkingTimeout(ctx, p.provider, p.providerName, p.modelName, req)
	if err == nil || !thinkingTimedOut {
		return resp, err
	}
	if p.tracker != nil {
		p.tracker.timeout.Store(true)
	}
	if recovered, ok := p.engine.recoverReasoningOnly(ctx, p.provider, req); ok {
		if p.tracker != nil {
			p.tracker.recovered.Store(true)
		}
		return &llm.CompletionResponse{Content: recovered}, nil
	}
	return resp, err
}

func (p *thinkingBoundProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	return p.provider.Stream(ctx, req)
}

func (p *thinkingBoundProvider) Models() []llm.ModelInfo {
	return p.provider.Models()
}

func (p *thinkingBoundProvider) CountTokens(messages []llm.Message) (int, error) {
	return p.provider.CountTokens(messages)
}

type runtimeToolExecutor struct {
	executor *ToolExecutor
}

func (e runtimeToolExecutor) Execute(ctx context.Context, call llm.ToolCall) (hruntime.ToolResult, error) {
	var args map[string]any
	if call.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			msg := fmt.Sprintf("Error: invalid arguments for tool %q: %s", call.Name, err.Error())
			return hruntime.ToolResult{Content: msg, Error: err.Error()}, nil
		}
	}
	if e.executor == nil {
		return hruntime.ToolResult{Content: "Error: tool executor not available", Error: "tool executor not available"}, nil
	}
	result, err := e.executor.Execute(ctx, call.Name, args)
	if err != nil {
		msg := fmt.Sprintf("Error executing tool %q: %s", call.Name, err.Error())
		return hruntime.ToolResult{Content: msg, Raw: result, Error: err.Error()}, nil
	}
	return hruntime.ToolResult{Content: result, Raw: result}, nil
}

type runtimeBudgetMiddleware struct {
	budget       *BudgetController
	providerName string
	modelName    string
}

func (m runtimeBudgetMiddleware) BeforeLLM(ctx context.Context, state *hruntime.State) error {
	if m.budget == nil {
		return nil
	}
	if err := m.budget.Check(); err != nil {
		trace.L(ctx).Warn("预算耗尽", "turn", state.Turn, "err", err)
		return err
	}
	return nil
}

func (m runtimeBudgetMiddleware) AfterLLM(_ context.Context, state *hruntime.State, resp *llm.CompletionResponse) error {
	if m.budget == nil || resp == nil || resp.Usage.TotalTokens <= 0 {
		return nil
	}
	providerName := m.providerName
	modelName := m.modelName
	if state != nil {
		if v, ok := state.Attributes["provider"].(string); ok && v != "" {
			providerName = v
		}
		if v, ok := state.Attributes["model"].(string); ok && v != "" {
			modelName = v
		}
	}
	m.budget.RecordTokens(resp.Usage.TotalTokens)
	if cost := EstimateCost(providerName, modelName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens); cost > 0 {
		m.budget.RecordCost(cost)
	}
	return nil
}

func (runtimeBudgetMiddleware) BeforeTool(context.Context, *hruntime.State, llm.ToolCall) error {
	return nil
}

func (runtimeBudgetMiddleware) AfterTool(context.Context, *hruntime.State, llm.ToolCall, hruntime.ToolResult) error {
	return nil
}

func (runtimeBudgetMiddleware) Finalize(context.Context, *hruntime.State) error { return nil }

type runtimeCompactionMiddleware struct {
	provider         hexagon.Provider
	providerForState func(*hruntime.State) hexagon.Provider
	sessionID        string
}

func (m runtimeCompactionMiddleware) BeforeLLM(ctx context.Context, state *hruntime.State) error {
	provider := m.provider
	if m.providerForState != nil {
		if p := m.providerForState(state); p != nil {
			provider = p
		}
	}
	state.Messages = compressContextIfNeeded(ctx, state.Messages, provider, m.sessionID)
	return nil
}

func (runtimeCompactionMiddleware) AfterLLM(context.Context, *hruntime.State, *llm.CompletionResponse) error {
	return nil
}

func (runtimeCompactionMiddleware) BeforeTool(context.Context, *hruntime.State, llm.ToolCall) error {
	return nil
}

func (runtimeCompactionMiddleware) AfterTool(context.Context, *hruntime.State, llm.ToolCall, hruntime.ToolResult) error {
	return nil
}

func (runtimeCompactionMiddleware) Finalize(context.Context, *hruntime.State) error { return nil }

func runtimeToolCallsToAdapter(calls []hruntime.ToolCallRecord) []adapter.ToolCall {
	result := make([]adapter.ToolCall, 0, len(calls))
	for _, c := range calls {
		result = append(result, adapter.ToolCall{
			ID:        c.ID,
			Name:      c.Name,
			Arguments: c.Arguments,
			Result:    stringx.TruncateWithSuffix(c.Result.Content, 500, "..."),
		})
	}
	return result
}
