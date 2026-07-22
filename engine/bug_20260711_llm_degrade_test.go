package engine

// BUG-20260711（真机反馈）：用户问“我的知识库有哪些文档”，模型侧失败时把原始技术
// 错误直接糊给用户：
//   - 云端：openrouter 免费 Nemotron 模型不支持工具调用 →
//     `No endpoints found that support tool use ... (code: 404)` 硬失败甩给用户。
//   - 本地：ollama `500 ... llama-server binary not found ... Run 'cmake ...'` 原始堆栈甩给用户。
//
// 期望不变量（模型/provider 无关，覆盖桌面非流式 + IM 流式两条路径）：
//   A. 模型明确“不支持工具调用”→ 去掉 tools 重试一次，对话正常出内容（降级而非硬失败）。
//   B. provider 硬错误（本地运行时缺 llama-server 等）→ 翻译成对用户友好、可操作的中文，
//      绝不把原始 500 / cmake / llama-server 堆栈泄漏给用户。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// errorsNew 造非空错误；errorsNewOrNil 对空串返回 nil（用于反例含 nil 判定）。
func errorsNew(s string) error { return errors.New(s) }
func errorsNewOrNil(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// degradeProvider 是模型/provider 无关的测试桩：Complete 与 Stream 共用同一 fn(len(tools))，
// fn 返回错误 → 两条路径都硬失败；返回内容 → Stream 走标准 OpenAI SSE delta 出内容（mockllm
// 默认 Stream 的 {"content":...} 形态在 OpenAI 解析下取不到内容，故这里自造正确 SSE）。
type degradeProvider struct {
	name string
	fn   func(toolCount int) (string, error)
}

func (p *degradeProvider) Name() string { return p.name }

func (p *degradeProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	content, err := p.fn(len(req.Tools))
	if err != nil {
		return nil, err
	}
	return &llm.CompletionResponse{Content: content, Usage: llm.Usage{TotalTokens: 8}}, nil
}

func (p *degradeProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	content, err := p.fn(len(req.Tools))
	if err != nil {
		return nil, err
	}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + content + "\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *degradeProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (p *degradeProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

// newDegradeEngine 装配一条带工具的活跃工具循环路径（tools 非空才走
// completeWithTools / processStreamRuntime）。
func newDegradeEngine(t *testing.T, provider hexagon.Provider) *ReActEngine {
	t.Helper()
	reg := skill.NewRegistry()
	if err := reg.Register(&echoSkill{}); err != nil {
		t.Fatal(err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, reg)
	eng.cfg.LLM.Tools.Enabled = "on"
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))
	return eng
}

func kbQueryMsg(id string) *adapter.Message {
	return &adapter.Message{
		ID:       id,
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "我的知识库有哪些文档",
	}
}

// RED-A：模型不支持工具调用 → 去工具重试，最终出正常内容，不把 404 甩给用户。
func TestBUG20260711_ToolUnsupported_DegradesToNoTools(t *testing.T) {
	const toolUnsupportedErr = "openrouter: No endpoints found that support tool use. To learn more about provider routing, visit: https://openrouter.ai/docs/provider-routing (code: 404)"
	const goodAnswer = "你的知识库有 3 个文档"

	newProvider := func() hexagon.Provider {
		return &degradeProvider{name: "test", fn: func(toolCount int) (string, error) {
			if toolCount > 0 {
				return "", errorsNew(toolUnsupportedErr)
			}
			return goodAnswer, nil
		}}
	}

	t.Run("NonStream", func(t *testing.T) {
		eng := newDegradeEngine(t, newProvider())
		reply, err := eng.Process(context.Background(), kbQueryMsg("degrade-a-nonstream"))
		if err != nil {
			t.Fatalf("降级后不应硬失败，实际 err=%v", err)
		}
		if !strings.Contains(reply.Content, goodAnswer) {
			t.Fatalf("降级后应出正常内容，实际 %q", reply.Content)
		}
		if strings.Contains(reply.Content, "404") || strings.Contains(reply.Content, "No endpoints") {
			t.Fatalf("正常内容里泄漏了原始 404 技术错误：%q", reply.Content)
		}
	})

	t.Run("Stream", func(t *testing.T) {
		eng := newDegradeEngine(t, newProvider())
		ch, err := eng.ProcessStream(context.Background(), kbQueryMsg("degrade-a-stream"))
		if err != nil {
			t.Fatalf("ProcessStream 启动失败: %v", err)
		}
		content, streamErr := drainStream(t, ch)
		if streamErr != nil {
			t.Fatalf("流式降级后不应回 error chunk，实际 %v", streamErr)
		}
		if !strings.Contains(content, goodAnswer) {
			t.Fatalf("流式降级后应出正常内容，实际 %q", content)
		}
		if strings.Contains(content, "404") || strings.Contains(content, "No endpoints") {
			t.Fatalf("流式内容泄漏了原始 404 技术错误：%q", content)
		}
	})
}

// RED-B：本地运行时缺组件 → 人性化中文，不甩原始 500 / llama-server / cmake 堆栈。
func TestBUG20260711_LocalRuntimeError_Humanized(t *testing.T) {
	const rawErr = "ollama api error: 500 Internal Server Error: llama runner process has terminated: llama-server binary not found (checked: /usr/lib/ollama). Run 'cmake --build --preset cpu' first"

	assertHumanized := func(t *testing.T, shown string) {
		t.Helper()
		for _, leak := range []string{"llama-server", "cmake", "500", "ollama api error"} {
			if strings.Contains(shown, leak) {
				t.Fatalf("用户可见错误泄漏原始技术细节 %q：%q", leak, shown)
			}
		}
		if !strings.Contains(shown, "本地模型未就绪") && !strings.Contains(shown, "切换") {
			t.Fatalf("用户可见错误应为友好可操作的中文，实际：%q", shown)
		}
	}

	newProvider := func() hexagon.Provider {
		return &degradeProvider{name: "test", fn: func(int) (string, error) {
			return "", errorsNew(rawErr)
		}}
	}

	t.Run("NonStream", func(t *testing.T) {
		eng := newDegradeEngine(t, newProvider())
		_, err := eng.Process(context.Background(), kbQueryMsg("degrade-b-nonstream"))
		if err == nil {
			t.Fatal("本地运行时缺组件应返回错误")
		}
		assertHumanized(t, err.Error())
	})

	t.Run("Stream", func(t *testing.T) {
		eng := newDegradeEngine(t, newProvider())
		ch, err := eng.ProcessStream(context.Background(), kbQueryMsg("degrade-b-stream"))
		if err != nil {
			t.Fatalf("ProcessStream 启动失败: %v", err)
		}
		_, streamErr := drainStream(t, ch)
		if streamErr == nil {
			t.Fatal("流式本地运行时缺组件应回 error chunk")
		}
		assertHumanized(t, streamErr.Error())
	})
}

// 单元锁：分类器 + 翻译器的正例/反例（含正常长英文不误判）。
func TestBUG20260711_isToolUnsupportedError(t *testing.T) {
	positives := []string{
		"openrouter: No endpoints found that support tool use (code: 404)",
		"model does not support tools",
		"this model does not support tool use",
		"tool use is not supported for this model",
		"tools are not supported by the selected endpoint",
		"function calling is not supported",
		"No endpoints found that support your request",
	}
	for _, s := range positives {
		if !isToolUnsupportedError(errorsNew(s)) {
			t.Errorf("应判为工具不支持：%q", s)
		}
	}
	negatives := []string{
		"",
		"connection reset by peer",
		"The assistant supported the user with several helpful tools during the conversation and everything worked.",
		"rate limit exceeded",
		"llama-server binary not found",
	}
	for _, s := range negatives {
		if isToolUnsupportedError(errorsNewOrNil(s)) {
			t.Errorf("不应判为工具不支持：%q", s)
		}
	}
}

func TestBUG20260711_isLocalRuntimeUnavailable(t *testing.T) {
	positives := []string{
		"llama runner process has terminated: llama-server binary not found",
		"error starting llama-server: exit status 1",
		"Run 'cmake --build --preset cpu' first",
	}
	for _, s := range positives {
		if !isLocalRuntimeUnavailable(errorsNew(s)) {
			t.Errorf("应判为本地运行时不可用：%q", s)
		}
	}
	negatives := []string{
		"",
		"No endpoints found that support tool use",
		"rate limit exceeded",
		"the cmaker finished building the wall",
	}
	for _, s := range negatives {
		if isLocalRuntimeUnavailable(errorsNewOrNil(s)) {
			t.Errorf("不应判为本地运行时不可用：%q", s)
		}
	}
}

func TestBUG20260711_friendlyLLMError(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		noleak []string
	}{
		{"local", "ollama api error: 500 llama-server binary not found. Run 'cmake --build'", "本地模型未就绪", []string{"llama-server", "cmake", "500"}},
		{"tool", "No endpoints found that support tool use (code: 404)", "不支持工具调用", []string{"404", "No endpoints"}},
		{"ratelimit", "429 Too Many Requests: rate limit exceeded", "频繁", []string{"429"}},
		{"auth", "401 Unauthorized: invalid api key", "鉴权", []string{"401", "api key"}},
		{"timeout", "context deadline exceeded", "超时", []string{"context deadline"}},
		{"default", "some totally unknown upstream failure xyz", "暂时不可用", []string{"xyz"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := friendlyLLMError(errorsNew(c.raw)).Error()
			if !strings.Contains(got, c.want) {
				t.Fatalf("friendlyLLMError(%q)=%q，应含 %q", c.raw, got, c.want)
			}
			for _, leak := range c.noleak {
				if strings.Contains(got, leak) {
					t.Fatalf("friendlyLLMError 泄漏原始细节 %q：%q", leak, got)
				}
			}
		})
	}
	if friendlyLLMError(nil) != nil {
		t.Fatal("friendlyLLMError(nil) 应返回 nil")
	}
}

func TestFriendlyLLMErrorPreservesContextSemanticsWithoutLeakingRawCause(t *testing.T) {
	tests := []struct {
		name        string
		raw         error
		wantCause   error
		wantMessage string
	}{
		{
			name:        "deadline",
			raw:         context.DeadlineExceeded,
			wantCause:   context.DeadlineExceeded,
			wantMessage: "模型响应超时，请稍后重试或切换到更快的模型。",
		},
		{
			name:        "wrapped deadline",
			raw:         fmt.Errorf("secret sk-test: %w", context.DeadlineExceeded),
			wantCause:   context.DeadlineExceeded,
			wantMessage: "模型响应超时，请稍后重试或切换到更快的模型。",
		},
		{
			name:        "cancelled",
			raw:         context.Canceled,
			wantCause:   context.Canceled,
			wantMessage: "模型服务暂时不可用，请稍后重试或切换模型。",
		},
		{
			name:        "wrapped cancelled",
			raw:         fmt.Errorf("secret sk-test: %w", context.Canceled),
			wantCause:   context.Canceled,
			wantMessage: "模型服务暂时不可用，请稍后重试或切换模型。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := friendlyLLMError(tt.raw)
			if !errors.Is(got, tt.wantCause) {
				t.Fatalf("friendly error lost context cause: got=%T %v want errors.Is(_, %v)", got, got, tt.wantCause)
			}
			if got.Error() != tt.wantMessage {
				t.Fatalf("friendly Error()=%q, want existing copy %q", got.Error(), tt.wantMessage)
			}
			if unwrapped := errors.Unwrap(got); unwrapped != tt.wantCause {
				t.Fatalf("friendly Unwrap()=%T %v, want only standard sentinel %v", unwrapped, unwrapped, tt.wantCause)
			}
			for _, rendered := range []string{
				got.Error(),
				fmt.Sprintf("%v", got),
				fmt.Sprintf("%+v", got),
				fmt.Sprintf("%#v", got),
				fmt.Errorf("outer: %w", got).Error(),
			} {
				for _, leak := range []string{"context deadline exceeded", "context canceled", "secret sk-test"} {
					if strings.Contains(rendered, leak) {
						t.Fatalf("friendly formatting leaked raw cause %q: %q", leak, rendered)
					}
				}
			}
		})
	}

	rateLimited := friendlyLLMError(errors.New("429 Too Many Requests: rate limit exceeded"))
	if rateLimited.Error() != "请求过于频繁，已被上游限流。请稍等片刻再试。" {
		t.Fatalf("rate-limit copy changed: %q", rateLimited.Error())
	}
	if errors.Is(rateLimited, context.DeadlineExceeded) || errors.Is(rateLimited, context.Canceled) || errors.Unwrap(rateLimited) != nil {
		t.Fatalf("ordinary rate-limit error must not acquire context semantics: %T %v", rateLimited, rateLimited)
	}
}
