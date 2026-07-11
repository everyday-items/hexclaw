package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestShouldForceCodeExecToolIntent(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "explicit code exec", text: "请调用 code_exec 运行 Python 脚本", want: true},
		{name: "python crawler", text: "执行一些网络爬虫 Python 试下", want: true},
		{name: "python shell task", text: "本应用模型写 Python shell 脚本来完成指定任务", want: true},
		{name: "code explanation only", text: "解释一下这段 Python 代码为什么慢", want: false},
		{name: "script review only", text: "帮我 review 这个 python 脚本", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldForceCodeExecTool(tt.text); got != tt.want {
				t.Fatalf("shouldForceCodeExecTool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyCodeExecToolChoice_ForcesInitialUserTurn(t *testing.T) {
	req := llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "请调用 code_exec 运行 Python"}},
		Tools:    []llm.ToolDefinition{llm.NewToolDefinition(codeExecToolName, "", nil)},
	}

	got := applyCodeExecToolChoice(req, "请调用 code_exec 运行 Python")
	if !isCodeExecToolChoice(got.ToolChoice) {
		t.Fatalf("ToolChoice = %#v, want forced code_exec function choice", got.ToolChoice)
	}
	if len(got.Messages) == 0 || !strings.Contains(got.Messages[0].Content, codeExecRequiredDirective) {
		t.Fatalf("required directive was not injected into system prompt: %#v", got.Messages)
	}
	if got.MaxTokens != codeExecInitialToolCallMaxTokens {
		t.Fatalf("MaxTokens = %d, want %d", got.MaxTokens, codeExecInitialToolCallMaxTokens)
	}
	if got.Metadata["thinking"] != "off" {
		t.Fatalf("thinking metadata = %#v, want off", got.Metadata["thinking"])
	}
	for _, want := range []string{`"mode":"snippet"`, `"language":"python"`, `"code":"<完整可运行脚本>"`, "禁止只传 mode/language/timeout"} {
		if !strings.Contains(got.Messages[0].Content, want) {
			t.Fatalf("required directive missing %q: %s", want, got.Messages[0].Content)
		}
	}
	params := got.Tools[0].Function.Parameters
	if params == nil {
		t.Fatal("code_exec parameters should be tightened for Python snippet request")
	}
	for _, want := range []string{"mode", "language", "code"} {
		if !stringSliceContains(params.Required, want) {
			t.Fatalf("tightened code_exec schema required=%v, missing %q", params.Required, want)
		}
	}
	if params.AdditionalProperties != false {
		t.Fatalf("tightened code_exec schema should reject undeclared fields, got %#v", params.AdditionalProperties)
	}
}

func TestApplyCodeExecToolChoice_KeepsGeneralSchemaForNonPython(t *testing.T) {
	original := &llm.Schema{Type: "object", Properties: map[string]*llm.Schema{"command": {Type: "array"}}}
	req := llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "请调用 code_exec 运行 Go 测试"}},
		Tools:    []llm.ToolDefinition{llm.NewToolDefinition(codeExecToolName, "", original)},
	}
	got := applyCodeExecToolChoice(req, "请调用 code_exec 运行 Go 测试")
	if got.Tools[0].Function.Parameters != original {
		t.Fatalf("non-Python code_exec request should keep general schema")
	}
}

func TestApplyCodeExecToolChoice_DoesNotForceAfterToolResult(t *testing.T) {
	req := llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "请调用 code_exec 运行 Python"},
			{Role: llm.RoleTool, Content: "PY_OK"},
		},
		Tools: []llm.ToolDefinition{llm.NewToolDefinition(codeExecToolName, "", nil)},
	}

	got := applyCodeExecToolChoice(req, "请调用 code_exec 运行 Python")
	if got.ToolChoice != nil {
		t.Fatalf("ToolChoice = %#v, want nil after tool result", got.ToolChoice)
	}
}

func TestCodeExecToolChoiceProvider_DelegatesWithForcedChoice(t *testing.T) {
	provider := &recordToolChoiceProvider{}
	wrapped := wrapCodeExecToolChoiceProvider(provider, "执行一些网络爬虫 Python 试下")

	_, err := wrapped.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "执行一些网络爬虫 Python 试下"}},
		Tools:    []llm.ToolDefinition{llm.NewToolDefinition(codeExecToolName, "", nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.last.ToolChoice == nil {
		t.Fatal("ToolChoice is nil, want forced code_exec tool choice")
	}
	if !isCodeExecToolChoice(provider.last.ToolChoice) {
		t.Fatalf("ToolChoice = %#v, want forced code_exec function choice", provider.last.ToolChoice)
	}
}

func TestRestrictToolsToCodeExecWhenRequired(t *testing.T) {
	tools := []llm.ToolDefinition{
		llm.NewToolDefinition("search", "", nil),
		llm.NewToolDefinition(codeExecToolName, "", nil),
		llm.NewToolDefinition("write_file", "", nil),
	}
	got := restrictToolsToCodeExecWhenRequired(tools, "请调用 code_exec 运行 Python")
	if len(got) != 1 || got[0].Function.Name != codeExecToolName {
		t.Fatalf("restricted tools = %#v, want only code_exec", got)
	}
	plain := restrictToolsToCodeExecWhenRequired(tools, "解释一下 Python 代码")
	if len(plain) != len(tools) {
		t.Fatalf("plain request should keep tools unchanged, got %d want %d", len(plain), len(tools))
	}
}

func TestExplicitCodeExecRequestsBypassSemanticCache(t *testing.T) {
	eng := newEngineWithProvider(t, newCountingProvider())
	ctx := context.Background()
	content := "请调用 code_exec 一次，自己写并运行 Python 脚本完成任务"

	first, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-code-exec-cache-1", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1", Content: content,
	})
	if err != nil {
		t.Fatalf("Process first failed: %v", err)
	}
	second, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-code-exec-cache-2", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1", Content: content,
	})
	if err != nil {
		t.Fatalf("Process second failed: %v", err)
	}
	if first.Content == second.Content {
		t.Fatalf("explicit code_exec request was served from semantic cache: first=%q second=%q", first.Content, second.Content)
	}
	if second.Metadata["source"] == "cache" {
		t.Fatal("explicit code_exec reply metadata claims semantic cache hit")
	}
	if hits := eng.LLMCache().Stats().Hits; hits != 0 {
		t.Fatalf("explicit code_exec cache bypass should run before cache.Get, got hits=%d", hits)
	}
}

func TestLiveStateRequestsBypassSemanticCache(t *testing.T) {
	tests := []struct {
		name string
		msg  *adapter.Message
		want bool
	}{
		{
			name: "knowledge file inventory",
			msg:  &adapter.Message{Content: "我的知识库有哪些文件？"},
			want: true,
		},
		{
			name: "local directory inventory",
			msg:  &adapter.Message{Content: "/Users/hexagon/work/ 目录下有哪些文件？"},
			want: true,
		},
		{
			name: "stateful k12 review queue",
			msg:  &adapter.Message{Content: "复习错题"},
			want: true,
		},
		{
			name: "stateful k12 grading",
			msg:  &adapter.Message{Content: "请批改这份作业"},
			want: true,
		},
		{
			name: "stateful record transition",
			msg:  &adapter.Message{Content: "这道题已经掌握了，标记一下"},
			want: true,
		},
		{
			name: "plain stable chat",
			msg:  &adapter.Message{Content: "1+1等于几"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldBypassSemanticCache(tt.msg); got != tt.want {
				t.Fatalf("shouldBypassSemanticCache() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSemanticCacheToolPurity(t *testing.T) {
	tests := []struct {
		name  string
		tools []string
		want  bool
	}{
		{name: "no tools", want: true},
		{name: "read only tools", tools: []string{"search", "knowledge_search", "weather"}, want: true},
		{name: "k12 review mutates records", tools: []string{"k12_review"}, want: false},
		{name: "k12 grade mutates records", tools: []string{"k12_grade"}, want: false},
		{name: "unknown defaults unsafe", tools: []string{"third_party_action"}, want: false},
		{name: "mixed purity", tools: []string{"search", "write_file"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := semanticCacheToolNamesCacheable(tt.tools); got != tt.want {
				t.Fatalf("semanticCacheToolNamesCacheable(%v) = %v, want %v", tt.tools, got, tt.want)
			}
		})
	}
}

type recordToolChoiceProvider struct {
	last llm.CompletionRequest
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (p *recordToolChoiceProvider) Name() string { return "record" }

func (p *recordToolChoiceProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.last = req
	return &llm.CompletionResponse{Content: "ok"}, nil
}

func (p *recordToolChoiceProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.last = req
	return nil, nil
}

func (p *recordToolChoiceProvider) Models() []llm.ModelInfo { return nil }

func (p *recordToolChoiceProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func isCodeExecToolChoice(choice any) bool {
	typed, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	fn, ok := typed["function"].(map[string]any)
	return ok && typed["type"] == "function" && fn["name"] == codeExecToolName
}
