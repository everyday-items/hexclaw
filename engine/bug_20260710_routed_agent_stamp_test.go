package engine

// BUG-20260710-H1（/review-go 审查 High）：skill.WithRoutedAgent 全仓唯一 stamp 点
// 在零调用点的死函数 processStreamToolLoop 里；两条活跃工具循环路径
// （Process→completeWithTools、ProcessStream→processStreamRuntime）均不 stamp。
// 后果：k12_grade / k12_review 在聊天工具链路上 skill.RoutedAgentName(ctx) 恒空
// → 要么 fail-loud「无法确定辅导实例」（功能残废），要么回退采信 LLM 传的
// agent 参数（多孩隔离被打穿——该回退由 skilladapter 侧同日测试一并封死）。
//
// 期望不变量：消息路由出 routed_agent（agent 路由器 / pinned agent 写入
// msg.Metadata["routed_agent"]）后，工具循环里每个 skill 的 Execute ctx 必须
// 能经 skill.RoutedAgentName(ctx) 取到该值——非流式与流式两条活跃路径都要。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// scopeProbeSkill 在 Execute 时记录 ctx 里的已路由 Agent 名。
type scopeProbeSkill struct {
	mu   sync.Mutex
	seen []string
}

func (s *scopeProbeSkill) Name() string        { return "scope_probe" }
func (s *scopeProbeSkill) Description() string { return "records routed agent scope" }
func (s *scopeProbeSkill) Match(string) bool   { return false }
func (s *scopeProbeSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.Name(), "probe", nil)
}
func (s *scopeProbeSkill) Execute(ctx context.Context, _ map[string]any) (*skill.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, skill.RoutedAgentName(ctx))
	return &skill.Result{Content: "ok"}, nil
}

func (s *scopeProbeSkill) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

type routedAgentProbeProvider struct{}

func (p *routedAgentProbeProvider) Name() string { return "test" }

func (p *routedAgentProbeProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.CompletionResponse{Content: "done"}, nil
		}
	}
	return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "scope_probe", Arguments: "{}"}}}, nil
}

func (p *routedAgentProbeProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"
			return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
		}
	}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"scope_probe\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *routedAgentProbeProvider) Models() []llm.ModelInfo                { return nil }
func (p *routedAgentProbeProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

// newRoutedAgentProbeEngine 装配：首轮回 tool_call(scope_probe)，次轮收敛为文本。
func newRoutedAgentProbeEngine(t *testing.T) (*ReActEngine, *scopeProbeSkill) {
	t.Helper()
	probe := &scopeProbeSkill{}
	reg := skill.NewRegistry()
	if err := reg.Register(probe); err != nil {
		t.Fatal(err)
	}
	provider := &routedAgentProbeProvider{}
	eng := newEngineWithProviderAndSkills(t, provider, reg)
	// Production wires both components in cmd/hexclaw. Keep the E2E fixture
	// faithful: without them the provider can emit a tool call, but the runtime
	// receives tools=0 and never reaches scopeProbeSkill.Execute.
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))
	return eng, probe
}

func routedMsg(id string) *adapter.Message {
	return &adapter.Message{
		ID:       id,
		Platform: adapter.PlatformAPI,
		UserID:   "u1",
		Content:  "检查作业",
		// 模拟 agent 路由器/pinned agent 已把消息路由到实例 mingming
		// （生产由 applyPinnedAgent / RouteWithFallback 写入同名键）。
		Metadata: map[string]string{"routed_agent": "mingming"},
	}
}

func assertProbeSawRoutedAgent(t *testing.T, probe *scopeProbeSkill, path string) {
	t.Helper()
	seen := probe.snapshot()
	if len(seen) == 0 {
		t.Fatalf("[%s] scope_probe 未被调用——mock provider 首轮应发 tool_call", path)
	}
	for _, got := range seen {
		if got != "mingming" {
			t.Fatalf("BUG 复现[%s]：工具执行 ctx 未携带已路由 Agent，skill.RoutedAgentName=%q（want %q）——"+
				"K12 实例 scope 在活跃路径上永远为空", path, got, "mingming")
		}
	}
}

func TestBUG20260710_H1_RoutedAgentReachesSkillCtx_NonStream(t *testing.T) {
	eng, probe := newRoutedAgentProbeEngine(t)
	if _, err := eng.Process(context.Background(), routedMsg("h1-nonstream")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	assertProbeSawRoutedAgent(t, probe, "Process/completeWithTools")
}

func TestBUG20260710_H1_RoutedAgentReachesSkillCtx_Stream(t *testing.T) {
	eng, probe := newRoutedAgentProbeEngine(t)
	ch, err := eng.ProcessStream(context.Background(), routedMsg("h1-stream"))
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	for chunk := range ch {
		if chunk != nil && chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
	}
	assertProbeSawRoutedAgent(t, probe, "ProcessStream/processStreamRuntime")
}
