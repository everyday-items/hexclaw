package engine

// BUG-20260712-#1 解题/批改用视觉默认模型漏判：K12 批改的 solver/verifier 子 Agent(solve 源)沿用
// Agent 的视觉默认模型(glm-4v-flash)。视觉模型擅长看图、不擅长多步文本解题 + 写 code_exec 验证代码，
// 真机复现:长方体体积题学生答 10(正确 30)被判成 correct=True/verdict=unverifiable → 漏判、错题入不了库。
//
// 修复:配置 reasoning_provider/reasoning_model(强文本模型如 智谱/glm-4.5)后,solve 源子 Agent 走它;
// 用户显式下发 provider/model 时不覆盖;未配则沿用默认路由(无回归)。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func newReasoningEngine(t *testing.T) *ReActEngine {
	t.Helper()
	p := &numCtxCaptureProvider{}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"智谱 AI": p},
		map[string]config.LLMProviderConfig{"智谱 AI": {Model: "glm-4v-flash"}}, // 默认=视觉模型
		"智谱 AI",
	)
	// 同一 provider,solve 源用模型覆盖到强文本模型(用户真实场景:一个智谱 provider,识题用 glm-4v-flash,批改用 glm-4.5)
	eng.cfg.LLM.ReasoningProvider = "智谱 AI"
	eng.cfg.LLM.ReasoningModel = "glm-4.5"
	return eng
}

// TestReasoningModel_SolveSourceUsesStrongTextModel solve 源(solver/verifier)→ 配置的推理模型 glm-4.5。
func TestReasoningModel_SolveSourceUsesStrongTextModel(t *testing.T) {
	eng := newReasoningEngine(t)
	msg := &adapter.Message{
		ID: "solve-sub", Platform: adapter.PlatformAPI, UserID: "system", Content: "5×3×2=?",
		Metadata: map[string]string{"source": solveDispatchSource, "role": solverAgentName},
	}
	sel, err := eng.resolveLLMSelection(context.Background(), msg)
	if err != nil {
		t.Fatalf("resolveLLMSelection: %v", err)
	}
	if sel.modelName != "glm-4.5" {
		t.Fatalf("BUG 复现:解题/批改未用配置的强文本模型 glm-4.5(仍用视觉默认→漏判),got model=%q provider=%q", sel.modelName, sel.providerName)
	}
	if sel.explicitProvider {
		t.Fatal("配置的 reasoning_model 只是系统首选，不是用户本轮显式 pin；硬失败时必须允许降级")
	}
}

// TestReasoningModel_NonSolveKeepsDefaultVisionModel 普通聊天/识题(非 solve 源)仍用默认视觉模型,不被误改。
func TestReasoningModel_NonSolveKeepsDefaultVisionModel(t *testing.T) {
	eng := newReasoningEngine(t)
	msg := &adapter.Message{
		ID: "chat-1", Platform: adapter.PlatformAPI, UserID: "u", Content: "你好",
	}
	sel, err := eng.resolveLLMSelection(context.Background(), msg)
	if err != nil {
		t.Fatalf("resolveLLMSelection: %v", err)
	}
	if sel.modelName != "glm-4v-flash" {
		t.Fatalf("非 solve 请求应保持默认视觉模型 glm-4v-flash,got %q", sel.modelName)
	}
}

// TestReasoningModel_ExplicitProviderNotOverridden solve 源但用户显式下发了 provider/model 时,尊重显式契约不覆盖。
func TestReasoningModel_ExplicitProviderNotOverridden(t *testing.T) {
	eng := newReasoningEngine(t)
	msg := &adapter.Message{
		ID: "solve-explicit", Platform: adapter.PlatformAPI, UserID: "system", Content: "x",
		Metadata: map[string]string{"source": solveDispatchSource, "provider": "智谱 AI", "model": "glm-4v-flash"},
	}
	sel, err := eng.resolveLLMSelection(context.Background(), msg)
	if err != nil {
		t.Fatalf("resolveLLMSelection: %v", err)
	}
	if sel.modelName != "glm-4v-flash" {
		t.Fatalf("显式下发的 model 应被尊重,不被推理模型覆盖,got %q", sel.modelName)
	}
}

// 热更新后若 reasoning_provider 悬空，绝不能返回 false 让 resolveLLMSelection 静默走
// 默认 provider（默认很可能是 Ollama）。这是配置错误，应在调用边界显式失败。
func TestReasoningModel_DanglingProviderFailsExplicitly(t *testing.T) {
	eng := newReasoningEngine(t)
	eng.cfg.LLM.ReasoningProvider = "provider-that-no-longer-exists"
	msg := &adapter.Message{
		ID: "solve-dangling", Platform: adapter.PlatformAPI, UserID: "system", Content: "1+1",
		Metadata: map[string]string{"source": solveDispatchSource, "role": solverAgentName},
	}

	_, err := eng.resolveLLMSelection(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "reasoning_provider") {
		t.Fatalf("dangling reasoning provider error=%v, want explicit reasoning_provider error", err)
	}
}
