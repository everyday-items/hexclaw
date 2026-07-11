package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/skill"
)

type egressCaptureProvider struct {
	mu       sync.Mutex
	requests [][]egress.Request
}

func (p *egressCaptureProvider) Name() string { return "egress-capture" }

func (p *egressCaptureProvider) capture(ctx context.Context) {
	reqs, _ := egress.RequestsFromContext(ctx)
	p.mu.Lock()
	p.requests = append(p.requests, reqs)
	p.mu.Unlock()
}

func (p *egressCaptureProvider) Complete(ctx context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.capture(ctx)
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}

func (p *egressCaptureProvider) Stream(ctx context.Context, _ hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.capture(ctx)
	body := strings.Join([]string{
		`data: {"id":"c1","model":"mock-model","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *egressCaptureProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Features: []string{llm.FeatureVision}}}
}

func (p *egressCaptureProvider) CountTokens([]llm.Message) (int, error) { return 1, nil }

func (p *egressCaptureProvider) last(t *testing.T) []egress.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider call did not carry an egress envelope")
	}
	return p.requests[len(p.requests)-1]
}

func requireEgressRequest(t *testing.T, reqs []egress.Request, purpose egress.Purpose, class egress.DataClass) {
	t.Helper()
	for _, req := range reqs {
		if req.Purpose == purpose && req.DataClass == class {
			return
		}
	}
	t.Fatalf("missing egress request purpose=%s class=%s; got %#v", purpose, class, reqs)
}

func requireNoEgressClass(t *testing.T, reqs []egress.Request, class egress.DataClass) {
	t.Helper()
	for _, req := range reqs {
		if req.DataClass == class {
			t.Fatalf("unexpected egress class %s in %#v", class, reqs)
		}
	}
}

func TestProcess_LabelsGeneralChatBeforeProviderBoundary(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	_, err := eng.Process(context.Background(), &adapter.Message{
		ID: "egress-sync", Platform: adapter.PlatformAPI, UserID: "u-egress", Content: "hello",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	requireEgressRequest(t, p.last(t), egress.PurposeGeneralChat, egress.ClassGeneral)
}

func TestProcessStream_LabelsGeneralChatBeforeProviderBoundary(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "egress-stream", Platform: adapter.PlatformAPI, UserID: "u-egress", Content: "hello",
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	for range ch {
	}
	requireEgressRequest(t, p.last(t), egress.PurposeGeneralChat, egress.ClassGeneral)
}

func TestBuildStreamMessages_AccumulatesInjectedDataClasses(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "audit", egress.ClassGeneral)
	eng.buildStreamMessages(ctx, "", nil, "retrieved document", "question", map[string]string{
		"agent_prompt": "private tutor profile",
	}, []adapter.Attachment{{Type: "image", Mime: "image/png", Data: "aGVsbG8="}})
	reqs, ok := egress.RequestsFromContext(ctx)
	if !ok {
		t.Fatal("egress envelope was lost")
	}
	// A custom agent/persona is an instruction, not a sensitive user profile.
	// Treating every persona as profile would block all cloud-backed custom agents.
	requireNoEgressClass(t, reqs, egress.ClassSensitiveProfile)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassSensitiveMedia)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassDocument)

	profileCtx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "audit-profile", egress.ClassGeneral)
	labelMessagePayloadEgress(profileCtx, &adapter.Message{Metadata: map[string]string{
		"profile_payload": "learner has dyslexia",
	}})
	profileReqs, _ := egress.RequestsFromContext(profileCtx)
	requireEgressRequest(t, profileReqs, egress.PurposeGeneralChat, egress.ClassSensitiveProfile)
}

func TestSolve_SubCallsUseIsolatedSolveVerifyEnvelope(t *testing.T) {
	var captured [][]egress.Request
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		reqs, _ := egress.RequestsFromContext(ctx)
		captured = append(captured, reqs)
		if spec.Agent == verifierAgentName {
			return SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 42"}, nil
		}
		return SubAgentResult{Output: "步骤：6×7=42\n答案：42"}, nil
	}
	_, err := NewSolveSkill(exec, nil).Execute(context.Background(), map[string]any{
		"problem": "看图计算 6×7", "grade": "五年级", "constraint": "只用整数乘法",
		"image_url": "https://example.invalid/problem.png", "self_consistency": float64(1),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(captured) < 2 {
		t.Fatalf("want solver and verifier calls, got %d", len(captured))
	}
	for i, reqs := range captured {
		requireEgressRequest(t, reqs, egress.PurposeSolveVerify, egress.ClassGeneral)
		requireEgressRequest(t, reqs, egress.PurposeSolveVerify, egress.ClassSensitiveProfile)
		requireEgressRequest(t, reqs, egress.PurposeSolveVerify, egress.ClassSensitiveMedia)
		for _, req := range reqs {
			if req.Purpose == egress.PurposeGeneralChat {
				t.Fatalf("solve call %d inherited general-chat purpose: %#v", i, reqs)
			}
		}
	}
}

func TestLabelMessageEgress_SolveRejectsWrongParentPurpose(t *testing.T) {
	parent := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "wrong-parent", egress.ClassRecord)
	ctx := labelMessageEgress(parent, &adapter.Message{Metadata: map[string]string{"source": solveDispatchSource}})
	reqs, _ := egress.RequestsFromContext(ctx)
	requireEgressRequest(t, reqs, egress.PurposeSolveVerify, egress.ClassGeneral)
	for _, req := range reqs {
		if req.Purpose != egress.PurposeSolveVerify {
			t.Fatalf("solve inherited wrong parent purpose: %#v", reqs)
		}
	}
}

func TestToolResultsAcquireTheirSpecificDataClass(t *testing.T) {
	tests := []struct {
		tool  string
		class egress.DataClass
	}{
		{tool: "knowledge_search", class: egress.ClassDocument},
		{tool: "session_search", class: egress.ClassMemory},
		{tool: "k12_grade", class: egress.ClassRecord},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "audit", egress.ClassGeneral)
			labelToolResultEgress(ctx, tt.tool)
			reqs, _ := egress.RequestsFromContext(ctx)
			requireEgressRequest(t, reqs, egress.PurposeGeneralChat, tt.class)
		})
	}
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "audit", egress.ClassGeneral)
	labelToolResultEgress(ctx, "solve")
	reqs, _ := egress.RequestsFromContext(ctx)
	requireNoEgressClass(t, reqs, egress.ClassRecord)
}

func TestJudgeText_LabelsOneShotProviderCall(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	if _, err := eng.JudgeText(context.Background(), "low or high?"); err != nil {
		t.Fatalf("JudgeText: %v", err)
	}
	requireEgressRequest(t, p.last(t), egress.PurposeGeneralChat, egress.ClassGeneral)
}

func TestJudgeText_PreservesParentSensitiveEnvelope(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "parent-audit", egress.ClassRecord)
	if _, err := eng.JudgeText(ctx, "review these tool args"); err != nil {
		t.Fatalf("JudgeText: %v", err)
	}
	reqs := p.last(t)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassRecord)
	for _, req := range reqs {
		if req.AuditID != "parent-audit" {
			t.Fatalf("JudgeText replaced parent audit id: %#v", reqs)
		}
	}
}

func TestCompleteOnce_DefaultsMissingEnvelopeButPreservesExplicitOne(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProvider(t, p)
	if _, err := eng.CompleteOnce(context.Background(), "system", "user", 8); err != nil {
		t.Fatalf("CompleteOnce default: %v", err)
	}
	requireEgressRequest(t, p.last(t), egress.PurposeGeneralChat, egress.ClassGeneral)

	ctx := egress.WithRequest(context.Background(), egress.PurposeSolveVerify, "solve-audit", egress.ClassSensitiveProfile)
	if _, err := eng.CompleteOnce(ctx, "system", "user", 8); err != nil {
		t.Fatalf("CompleteOnce explicit: %v", err)
	}
	reqs := p.last(t)
	requireEgressRequest(t, reqs, egress.PurposeSolveVerify, egress.ClassSensitiveProfile)
	for _, req := range reqs {
		if req.AuditID != "solve-audit" {
			t.Fatalf("CompleteOnce replaced explicit envelope: %#v", reqs)
		}
	}
}

func TestWarmupLocalDefaultModel_LabelsStaticGeneralRequest(t *testing.T) {
	p := &egressCaptureProvider{}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"ollama": p},
		map[string]config.LLMProviderConfig{"ollama": {Model: "mock-model"}},
		"ollama",
	)
	warmed, err := eng.WarmupLocalDefaultModel(context.Background())
	if err != nil {
		t.Fatalf("WarmupLocalDefaultModel: %v", err)
	}
	if !warmed {
		t.Fatal("ollama route should execute warmup")
	}
	requireEgressRequest(t, p.last(t), egress.PurposeGeneralChat, egress.ClassGeneral)
}

type egressRecordProbeSkill struct{}

func (*egressRecordProbeSkill) Name() string        { return "k12_record_probe" }
func (*egressRecordProbeSkill) Description() string { return "returns a domain record" }
func (*egressRecordProbeSkill) Match(string) bool   { return false }
func (s *egressRecordProbeSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.Name(), s.Description(), nil)
}
func (*egressRecordProbeSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: `{"record":"private"}`}, nil
}

type egressToolCaptureProvider struct{ egressCaptureProvider }

func (p *egressToolCaptureProvider) Complete(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.capture(ctx)
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &hexagon.CompletionResponse{Content: "done"}, nil
		}
	}
	return &hexagon.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "record-call", Name: "k12_record_probe", Arguments: `{}`}}}, nil
}

func (p *egressToolCaptureProvider) Stream(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.capture(ctx)
	hasToolResult := false
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			hasToolResult = true
			break
		}
	}
	body := ""
	if hasToolResult {
		body = `data: {"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n"
	} else {
		body = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"record-call","type":"function","function":{"name":"k12_record_probe","arguments":"{}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n"
	}
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func newEgressToolEngine(t *testing.T, provider hexagon.Provider) *ReActEngine {
	t.Helper()
	reg := skill.NewRegistry()
	if err := reg.Register(&egressRecordProbeSkill{}); err != nil {
		t.Fatal(err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, reg)
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))
	return eng
}

func TestProcess_ToolResultClassifiedBeforeSecondProviderCall(t *testing.T) {
	p := &egressToolCaptureProvider{}
	eng := newEgressToolEngine(t, p)
	_, err := eng.Process(context.Background(), &adapter.Message{
		ID: "record-sync", Platform: adapter.PlatformAPI, UserID: "u-record", Content: "run record probe",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	p.mu.Lock()
	calls := append([][]egress.Request(nil), p.requests...)
	p.mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("provider calls=%d, want tool request + final request", len(calls))
	}
	requireNoEgressClass(t, calls[0], egress.ClassRecord)
	requireEgressRequest(t, calls[len(calls)-1], egress.PurposeGeneralChat, egress.ClassRecord)
}

func TestProcessStream_ToolResultClassifiedBeforeSecondProviderCall(t *testing.T) {
	p := &egressToolCaptureProvider{}
	eng := newEgressToolEngine(t, p)
	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "record-stream", Platform: adapter.PlatformAPI, UserID: "u-record", Content: "run record probe",
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	for range ch {
	}
	p.mu.Lock()
	calls := append([][]egress.Request(nil), p.requests...)
	p.mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("provider calls=%d, want tool request + final request", len(calls))
	}
	requireNoEgressClass(t, calls[0], egress.ClassRecord)
	requireEgressRequest(t, calls[len(calls)-1], egress.PurposeGeneralChat, egress.ClassRecord)
}
