package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type pinnedAgentRouteCaptureProvider struct {
	name                    string
	content                 string
	err                     error
	publishReasoningReceipt bool
	reasoningApplication    llm.ReasoningApplication

	mu       sync.Mutex
	requests []pinnedAgentRouteRequest
}

type pinnedAgentRouteRequest struct {
	model      string
	thinking   string
	capability llm.ReasoningCapability
}

func (p *pinnedAgentRouteCaptureProvider) Name() string { return p.name }

func (p *pinnedAgentRouteCaptureProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.recordRequest(req)
	p.publishReceipt(req)
	if p.err != nil {
		return nil, p.err
	}
	return &llm.CompletionResponse{
		Content: p.content,
		Usage:   llm.Usage{TotalTokens: 1},
	}, nil
}

func (p *pinnedAgentRouteCaptureProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.recordRequest(req)
	p.publishReceipt(req)
	if p.err != nil {
		return nil, p.err
	}
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]string{"content": p.content}}},
	})
	return llm.NewStream(
		strings.NewReader("data: "+string(payload)+"\n\ndata: [DONE]\n\n"),
		llm.StreamOpenAIFormat,
	), nil
}

func (p *pinnedAgentRouteCaptureProvider) Models() []llm.ModelInfo { return nil }

func (p *pinnedAgentRouteCaptureProvider) CountTokens(messages []llm.Message) (int, error) {
	return len(messages), nil
}

func (p *pinnedAgentRouteCaptureProvider) recordRequest(req llm.CompletionRequest) {
	capability, _ := req.Metadata[llm.ReasoningCapabilityMetadataKey].(llm.ReasoningCapability)
	thinking, _ := req.Metadata["thinking"].(string)
	p.mu.Lock()
	p.requests = append(p.requests, pinnedAgentRouteRequest{
		model:      req.Model,
		thinking:   strings.TrimSpace(thinking),
		capability: capability,
	})
	p.mu.Unlock()
}

func (p *pinnedAgentRouteCaptureProvider) calledModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	models := make([]string, 0, len(p.requests))
	for _, request := range p.requests {
		models = append(models, request.model)
	}
	return models
}

func (p *pinnedAgentRouteCaptureProvider) calledRequests() []pinnedAgentRouteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pinnedAgentRouteRequest(nil), p.requests...)
}

func (p *pinnedAgentRouteCaptureProvider) publishReceipt(req llm.CompletionRequest) {
	if !p.publishReasoningReceipt {
		return
	}
	application := p.reasoningApplication
	if application == "" {
		application = llm.ReasoningApplicationRejected
	}
	applied := application == llm.ReasoningApplicationApplied
	llm.PublishReasoningReceipt(req.Metadata, llm.ReasoningReceipt{
		Version:     1,
		Enabled:     true,
		Support:     llm.ReasoningSupported,
		Dialect:     llm.ReasoningDialectEffort,
		Sent:        true,
		Accepted:    true,
		Observed:    applied,
		Applied:     applied,
		Application: application,
	})
}

// 绑定命名 Agent 本身就是显式固定路由。即使 Desktop 只发送 pinned_agent，
// 绑定模型不可用时也不能改用同 Provider 默认模型或跨 Provider。
func TestPinnedAgentExactRouteUnavailableHasZeroFallbackCalls(t *testing.T) {
	bound := &pinnedAgentRouteCaptureProvider{
		name: "bound-provider",
		err:  errors.New("503 Service Unavailable"),
	}
	other := &pinnedAgentRouteCaptureProvider{
		name:    "other-provider",
		content: "fallback must not run",
	}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{
			bound.name: bound,
			other.name: other,
		},
		map[string]config.LLMProviderConfig{
			bound.name: {
				Model:          "bound-default-model",
				Models:         []string{"bound-default-model", "bound-agent-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "bound-default-model", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "bound-agent-model", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
			other.name: {
				Model:          "other-default-model",
				Models:         []string{"other-default-model", "bound-agent-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "other-default-model", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "bound-agent-model", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
		},
		other.name,
	)

	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:     "tutor",
		Provider: bound.name,
		Model:    "bound-agent-model",
	}); err != nil {
		t.Fatalf("注册固定 Agent 失败: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	msg := &adapter.Message{
		ID:       "pinned-agent-fixed-route",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		ChatID:   "chat-1",
		Content:  "请讲解这道数学题",
		Metadata: map[string]string{
			"pinned_agent": "tutor",
		},
	}
	chunks, terminalErr := eng.ProcessStream(context.Background(), msg)
	if terminalErr == nil {
		for chunk := range chunks {
			if chunk.Error != nil {
				terminalErr = chunk.Error
			}
		}
	}

	if terminalErr == nil {
		t.Error("固定 Agent 的精确路由不可用时必须返回原路由错误，不能由 fallback 静默成功")
	}
	if got, want := bound.calledModels(), []string{"bound-agent-model"}; !equalStrings(got, want) {
		t.Errorf("绑定 Provider 物理调用模型=%v，期望仅调用精确模型 %v（默认模型调用必须为 0）", got, want)
	}
	if got := other.calledModels(); len(got) != 0 {
		t.Errorf("跨 Provider 物理调用=%v，期望为 0", got)
	}
}

// solve 派发仍可能只携带 Desktop 冻结的 pinned_agent。命名 Agent 的权威路由
// 必须优先于全局 reasoning 首选解析，否则首选失败后会被当成非显式路由降级。
func TestPinnedAgentExactRouteSolveDispatchUnavailableHasZeroFallbackCalls(t *testing.T) {
	bound := &pinnedAgentRouteCaptureProvider{
		name:                    "bound-provider",
		err:                     errors.New("503 Service Unavailable"),
		publishReasoningReceipt: true,
		reasoningApplication:    llm.ReasoningApplicationRejected,
	}
	other := &pinnedAgentRouteCaptureProvider{
		name:    "other-provider",
		content: "fallback must not run",
	}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{
			bound.name: bound,
			other.name: other,
		},
		map[string]config.LLMProviderConfig{
			bound.name: {
				Model:          "bound-default-model",
				Models:         []string{"bound-default-model", "bound-agent-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{
						ID:               "bound-default-model",
						Capabilities:     []string{config.LLMModelCapabilityText},
						ReasoningSupport: config.LLMReasoningSupportSupported,
						ReasoningControl: &config.LLMReasoningControlSpec{
							Dialect: config.LLMReasoningDialectThinking,
							On:      true,
							Off:     false,
						},
					},
					{
						ID:               "bound-agent-model",
						Capabilities:     []string{config.LLMModelCapabilityText},
						ReasoningSupport: config.LLMReasoningSupportSupported,
						ReasoningControl: &config.LLMReasoningControlSpec{
							Dialect:        config.LLMReasoningDialectEffort,
							On:             "low",
							Off:            "none",
							AllowedEfforts: []string{"low"},
						},
					},
				},
			},
			other.name: {
				Model:          "other-default-model",
				Models:         []string{"other-default-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "other-default-model", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
		},
		other.name,
	)
	eng.cfg.LLM.ReasoningProvider = bound.name
	eng.cfg.LLM.ReasoningModel = "bound-agent-model"

	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:     "tutor",
		Provider: bound.name,
		Model:    "bound-agent-model",
	}); err != nil {
		t.Fatalf("注册固定 Agent 失败: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	msg := &adapter.Message{
		ID:       "pinned-agent-fixed-solve-route",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		ChatID:   "chat-1",
		Content:  "请讲解这道数学题",
		Metadata: map[string]string{
			"pinned_agent": "tutor",
			"source":       solveDispatchSource,
			"thinking":     "on",
		},
	}
	chunks, terminalErr := eng.ProcessStream(context.Background(), msg)
	if terminalErr == nil {
		for chunk := range chunks {
			if chunk.Error != nil {
				terminalErr = chunk.Error
			}
		}
	}

	if terminalErr == nil {
		t.Error("固定 Agent 的精确 solve 路由不可用时必须返回原路由错误，不能由 fallback 静默成功")
	} else if want := friendlyLLMError(bound.err); terminalErr.Error() != want.Error() || strings.Contains(terminalErr.Error(), other.name) {
		t.Errorf("错误=%q，期望稳定保留原精确路由错误且不归因到 fallback", terminalErr)
	}
	if got, want := bound.calledModels(), []string{"bound-agent-model"}; !equalStrings(got, want) {
		t.Errorf("绑定 Provider 物理调用模型=%v，期望仅调用精确模型 %v（默认模型调用必须为 0）", got, want)
	}
	if got := other.calledModels(); len(got) != 0 {
		t.Errorf("跨 Provider 物理调用=%v，期望为 0", got)
	}
	if got := msg.Metadata["routed_agent"]; got != "tutor" {
		t.Errorf("routed_agent=%q，期望先冻结为 tutor", got)
	}
	requests := bound.calledRequests()
	if len(requests) != 1 {
		t.Fatalf("精确路由请求数=%d，期望 1", len(requests))
	}
	request := requests[0]
	if request.model != "bound-agent-model" || request.thinking != "on" ||
		request.capability.Support != llm.ReasoningSupported ||
		request.capability.Dialect != llm.ReasoningDialectEffort ||
		request.capability.OnValue != "low" {
		t.Errorf("精确路由 request/capability 快照=%+v，期望 bound-agent-model + effort/low", request)
	}

	bound.err = nil
	bound.content = "exact route reply"
	bound.reasoningApplication = llm.ReasoningApplicationApplied
	success := *msg
	success.ID = "pinned-agent-fixed-solve-route-success"
	success.ChatID = "chat-2"
	success.SessionID = ""
	success.Metadata = map[string]string{
		"pinned_agent": "tutor",
		"source":       solveDispatchSource,
		"thinking":     "on",
	}
	chunks, terminalErr = eng.ProcessStream(context.Background(), &success)
	var disclosure adapter.ReasoningDisclosure
	var receipt *adapter.ReasoningReceipt
	if terminalErr == nil {
		for chunk := range chunks {
			if chunk.ReasoningDisclosure.Provider != "" {
				disclosure = chunk.ReasoningDisclosure
			}
			if chunk.ReasoningReceipt != nil {
				copy := *chunk.ReasoningReceipt
				receipt = &copy
			}
			if chunk.Error != nil {
				terminalErr = chunk.Error
			}
		}
	}
	if terminalErr != nil {
		t.Fatalf("精确路由成功请求失败: %v", terminalErr)
	}
	requests = bound.calledRequests()
	if len(requests) != 2 || requests[1].model != "bound-agent-model" ||
		requests[1].capability.Dialect != llm.ReasoningDialectEffort || requests[1].capability.OnValue != "low" {
		t.Errorf("成功请求未复用同一精确 request/capability 快照: %+v", requests)
	}
	if disclosure.Provider != bound.name || disclosure.Model != "bound-agent-model" {
		t.Errorf("reasoning disclosure 路由=(%q,%q)，期望精确快照 (%q,%q)", disclosure.Provider, disclosure.Model, bound.name, "bound-agent-model")
	}
	if receipt == nil || receipt.ReasoningRequest != adapter.ReasoningRequestOn ||
		receipt.ReasoningSupport != adapter.ReasoningSupportSupported ||
		receipt.ReasoningExecution != adapter.ReasoningExecutionApplied {
		t.Errorf("reasoning receipt=%+v，期望同一精确路由的 on/supported/applied", receipt)
	}
	if got := other.calledModels(); len(got) != 0 {
		t.Errorf("成功请求跨 Provider 物理调用=%v，期望为 0", got)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
