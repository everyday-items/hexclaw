package engine

import (
	"context"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	"github.com/hexagon-codes/hexclaw/adapter"
)

type codeExecToolChoiceProvider struct {
	provider    hexagon.Provider
	userContent string
}

func wrapCodeExecToolChoiceProvider(provider hexagon.Provider, userContent string) hexagon.Provider {
	if provider == nil || !shouldForceCodeExecTool(userContent) {
		return provider
	}
	return &codeExecToolChoiceProvider{provider: provider, userContent: userContent}
}

func (p *codeExecToolChoiceProvider) Name() string {
	return p.provider.Name()
}

func (p *codeExecToolChoiceProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	req = applyCodeExecToolChoice(req, p.userContent)
	return p.provider.Complete(ctx, req)
}

func (p *codeExecToolChoiceProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	req = applyCodeExecToolChoice(req, p.userContent)
	return p.provider.Stream(ctx, req)
}

func (p *codeExecToolChoiceProvider) Models() []llm.ModelInfo {
	return p.provider.Models()
}

func (p *codeExecToolChoiceProvider) CountTokens(messages []llm.Message) (int, error) {
	return p.provider.CountTokens(messages)
}

func applyCodeExecToolChoice(req llm.CompletionRequest, userContent string) llm.CompletionRequest {
	if !shouldApplyInitialCodeExecToolChoice(req, userContent) {
		return req
	}
	req.Messages = injectCodeExecRequiredDirective(req.Messages)
	req.Tools = tightenCodeExecInitialToolSchema(req.Tools, userContent)
	req.ToolChoice = map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": codeExecToolName,
		},
	}
	temperature := 0.0
	req.Temperature = &temperature
	topP := 0.1
	req.TopP = &topP
	if req.MaxTokens <= 0 || req.MaxTokens > codeExecInitialToolCallMaxTokens {
		req.MaxTokens = codeExecInitialToolCallMaxTokens
	}
	if req.Metadata == nil {
		req.Metadata = make(map[string]any, 1)
	}
	req.Metadata["thinking"] = "off"
	return req
}

func shouldApplyInitialCodeExecToolChoice(req llm.CompletionRequest, userContent string) bool {
	if !shouldForceCodeExecTool(userContent) || !completionRequestHasTool(req.Tools, codeExecToolName) {
		return false
	}
	if req.ToolChoice != nil && req.ToolChoice != "auto" {
		return false
	}
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	return last.Role == llm.RoleUser
}

func shouldBypassSemanticCache(msg *adapter.Message) bool {
	if isSystemDispatch(msg) {
		return true
	}
	if msg == nil {
		return false
	}
	return shouldForceCodeExecTool(msg.Content) || shouldBypassLiveStateCache(msg.Content)
}

func hasCodeExecAdapterCall(calls []adapter.ToolCall) bool {
	for _, call := range calls {
		if call.Name == codeExecToolName {
			return true
		}
	}
	return false
}

func hasCodeExecRuntimeCall(calls []hruntime.ToolCallRecord) bool {
	for _, call := range calls {
		if call.Name == codeExecToolName {
			return true
		}
	}
	return false
}

func hasCodeExecLLMCall(calls []llm.ToolCall) bool {
	for _, call := range calls {
		if call.Name == codeExecToolName {
			return true
		}
	}
	return false
}

func hasCodeExecStreamCall(calls []streamx.ToolCall) bool {
	for _, call := range calls {
		if call.Name == codeExecToolName {
			return true
		}
	}
	return false
}

func completionRequestHasTool(tools []llm.ToolDefinition, name string) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func restrictToolsToCodeExecWhenRequired(tools []llm.ToolDefinition, content string) []llm.ToolDefinition {
	if !shouldForceCodeExecTool(content) {
		return tools
	}
	for _, tool := range tools {
		if tool.Function.Name == codeExecToolName {
			return []llm.ToolDefinition{tool}
		}
	}
	return tools
}

func tightenCodeExecInitialToolSchema(tools []llm.ToolDefinition, content string) []llm.ToolDefinition {
	if !shouldUsePythonSnippetSchema(content) {
		return tools
	}
	out := append([]llm.ToolDefinition(nil), tools...)
	for i := range out {
		if out[i].Function.Name == codeExecToolName {
			out[i] = pythonSnippetCodeExecTool(out[i])
		}
	}
	return out
}

func pythonSnippetCodeExecTool(tool llm.ToolDefinition) llm.ToolDefinition {
	tool.Function.Description = strings.TrimSpace(tool.Function.Description + " For this Python script request, call once with mode=snippet, language=python, and complete code.")
	tool.Function.Parameters = &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"mode": {
				Type:        "string",
				Description: "Must be snippet for this request.",
				Enum:        []any{"snippet"},
			},
			"language": {
				Type:        "string",
				Description: "Must be python or python3 for this request.",
				Enum:        []any{"python", "python3"},
			},
			"code": {
				Type:        "string",
				Description: "Complete runnable Python script. Required.",
			},
			"timeout": {
				Type:        "integer",
				Description: "Optional timeout in seconds.",
			},
		},
		Required:             []string{"mode", "language", "code"},
		AdditionalProperties: false,
	}
	return tool
}

func shouldUsePythonSnippetSchema(content string) bool {
	s := strings.ToLower(strings.TrimSpace(content))
	return strings.Contains(s, "python") || strings.Contains(s, "py脚本")
}

const codeExecInitialToolCallMaxTokens = 2048

const codeExecRequiredDirective = "/no_think\n当前用户请求明确要求执行代码。你必须立即调用 code_exec 工具完成执行；本轮不要输出自然语言答案，不要展开推理，不要声称已运行。Python/Shell 脚本任务必须一次性传完整参数：{\"mode\":\"snippet\",\"language\":\"python\",\"code\":\"<完整可运行脚本>\",\"timeout\":30}；禁止只传 mode/language/timeout 的空调用。只有拿到 code_exec 工具结果后，才能基于 stdout/stderr 给最终回答。"

func injectCodeExecRequiredDirective(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return []llm.Message{{Role: llm.RoleSystem, Content: codeExecRequiredDirective}}
	}
	out := append([]llm.Message(nil), messages...)
	for i := range out {
		if out[i].Role == llm.RoleSystem {
			if !strings.Contains(out[i].Content, codeExecRequiredDirective) {
				out[i].Content = strings.TrimSpace(out[i].Content) + "\n\n" + codeExecRequiredDirective
			}
			return out
		}
	}
	return append([]llm.Message{{Role: llm.RoleSystem, Content: codeExecRequiredDirective}}, out...)
}

// codeExecIntent 是 code_exec 强制意图的 typed 判定结果。Force 命中即强制走 code_exec
// tool_choice（剥掉其余工具）；Reason 记录命中/放行的依据，便于观测与去重消费。
type codeExecIntent struct {
	Force  bool
	Reason string
}

// classifyCodeExecIntent 是 code_exec 强制意图的**单一判定入口**。
//
// 一次判定，多处消费同一结果（shouldForceCodeExecTool / restrict / bypass-cache 等均经此）。
// 收紧误触发：先排除「元讨论 / how-to」问句（how do I / how to / 怎么 / 如何 / safely /
// 最佳实践 …），这些是「讨论如何做」而非「现在就执行」，绝不强制——不强制≠禁止，模型仍可
// 经 tool_choice=auto 自选 code_exec，只是不再被剥掉其余工具。随后仅在出现明确的祈使
// 执行意图或强信号组合时才 Force，避免单个动词子串误命中。
func classifyCodeExecIntent(content string) codeExecIntent {
	s := strings.ToLower(strings.TrimSpace(content))
	if s == "" {
		return codeExecIntent{}
	}
	// 元讨论 / how-to 前缀 → 不强制（收口误触发的核心）。
	if isCodeExecMetaDiscussion(s) {
		return codeExecIntent{Force: false, Reason: "meta-discussion/how-to"}
	}
	compact := strings.NewReplacer(" ", "", "\n", "", "\t", "", "-", "_").Replace(s)
	if strings.Contains(compact, "codeexec") {
		return codeExecIntent{Force: true, Reason: "explicit code_exec mention"}
	}
	// 明确的祈使执行意图（整词短语，而非单个动词子串）。
	if containsAny(s, "执行代码", "运行代码", "跑代码", "execute code", "run code") ||
		containsAny(compact, "执行这段代码", "运行这段代码", "跑这段代码", "执行以下代码", "运行以下代码", "执行下面的代码") ||
		containsAny(s, "run this code", "execute this code", "run the following code", "execute the following code") {
		return codeExecIntent{Force: true, Reason: "imperative execute-code"}
	}

	hasPython := strings.Contains(s, "python") || strings.Contains(s, "py脚本")
	hasShell := strings.Contains(s, "shell") || strings.Contains(s, "bash")
	hasExec := containsAny(s,
		"执行", "运行", "跑一下", "跑一遍", "跑通", "试下", "测试一下",
		"run ", "execute ", "exec ",
	)
	hasCrawler := containsAny(s,
		"爬虫", "爬取", "抓取", "采集网页", "网页抓取", "网络抓取",
		"crawler", "crawl", "scrape", "scraping", "requests",
	)
	hasWorkVerb := containsAny(s, "完成", "计算", "处理", "统计", "校验", "解析", "生成结果")

	if hasPython && hasExec {
		return codeExecIntent{Force: true, Reason: "python+exec"}
	}
	if hasPython && hasCrawler {
		return codeExecIntent{Force: true, Reason: "python+crawler"}
	}
	if hasCrawler && containsAny(s, "http://", "https://", "网页", "网站", "页面") {
		return codeExecIntent{Force: true, Reason: "crawler+url"}
	}
	if hasPython && hasShell && hasWorkVerb {
		return codeExecIntent{Force: true, Reason: "python+shell+work"}
	}
	return codeExecIntent{}
}

// isCodeExecMetaDiscussion 判定内容是否为「讨论/询问如何做」的元话语（而非执行指令）。
// 这些问句里即便出现 "run code" 之类子串，也不应被强制到 code_exec。
func isCodeExecMetaDiscussion(s string) bool {
	return containsAny(s,
		"how do i", "how to", "how can i", "how should", "how would",
		"怎么", "如何", "怎样", "是否可以", "能不能", "可不可以",
		"safely", "safe way", "best practice", "最佳实践", "安全地",
		"是什么", "什么是", "为什么",
	)
}

// shouldForceCodeExecTool 是布尔消费点，转发到单一判定入口 classifyCodeExecIntent。
func shouldForceCodeExecTool(content string) bool {
	return classifyCodeExecIntent(content).Force
}

func shouldBypassLiveStateCache(content string) bool {
	s := strings.ToLower(strings.TrimSpace(content))
	if s == "" {
		return false
	}
	fileInventoryIntent := containsAny(s,
		"有哪些文件", "哪些文件", "列出文件", "列一下文件", "文件列表",
		"目录下", "一级目录", "list files", "list directory", "ls ",
	)
	if !fileInventoryIntent {
		return false
	}
	if containsAny(s, "知识库", "knowledge base", "kb") {
		return true
	}
	return containsAny(s,
		"/users/", "/private/", "~/", "/tmp/", "/var/",
		"本地文件", "本地目录", "文件夹", "目录", "directory", "folder",
	)
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
