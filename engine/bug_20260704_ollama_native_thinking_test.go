package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// reasoningStreamProvider 模拟原生 Ollama thinking 模型：
//   - Stream：像 ai-core ollama adapter 一样，把 message.thinking 作为 reasoning 增量分离透出。
//   - Complete（非流式）：像 adapter.parseResponse 一样**丢弃 thinking**，只回正文。
//
// 这样它能精确复现「深度思考开启时若走非流式有界补全→推理被丢」的缺陷。
type reasoningStreamProvider struct {
	name          string
	completeCalls int
	streamCalls   int
}

func (p *reasoningStreamProvider) Name() string { return p.name }

func (p *reasoningStreamProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.completeCalls++
	// 非流式路径丢弃推理（复现 ollama parseResponse 不拷贝 message.thinking）。
	return &llm.CompletionResponse{Content: "最终答案：42"}, nil
}

func (p *reasoningStreamProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.streamCalls++
	// 流式路径把推理作为 reasoning_content 增量分离透出（原生 think:true 的真实形态）。
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"我先想一下这道题的思路…\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"最终答案：42\"}}]}\n\n" +
		"data: [DONE]\n\n"
	stream := llm.NewStream(strings.NewReader(sse), llm.StreamOpenAIFormat)
	stream.SetParser(&streamx.OpenAIParser{
		ReasoningEvidence: streamx.ReasoningDisclosureEvidence{
			ExplicitlyPublic: true,
			Provider:         p.name,
			Model:            req.Model,
		},
	})
	return stream, nil
}

func (p *reasoningStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "qwen3.5:9b", Name: "qwen3.5:9b"}}
}

func (p *reasoningStreamProvider) CountTokens(messages []llm.Message) (int, error) {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
	}
	return total, nil
}

// BUG-20260704：本地 Ollama thinking 模型（qwen3.5:9b）在桌面聊天里「几分钟才回复 +
// 不显示推理」。根因=本地 thinking 控制没有把用户的「深度思考」意图确定性地翻成
// 原生 `think` 参数（ai-core ollama adapter 读 req.Metadata["thinking"] → payload.think）。
// think 若留空，Ollama 对 qwen3.5 默认进入「思考但不分离」模式——既慢（隐藏思考几千 token）
// 又不显示推理（thinking 字段不产出 → reasoning_chunks=0）。
//
// 契约（本测试钉死）：本地 thinking 模型请求到达 provider 时，req.Metadata["thinking"]
// 必须是**显式** "on"/"off"（绝不留空），由用户「深度思考」开关决定：
//   - 默认（未开深度思考）→ "off"：原生 think:false，快，且不夹带隐藏思考；
//   - 开深度思考（msg.Metadata["thinking"]="on"）→ "on"：原生 think:true，推理可显示。
//
// 这条契约覆盖**带工具**路径（桌面聊天恒挂 28 工具）——旧 /no_think 注入被 len(Tools)==0
// 挡住，对带工具场景失效，是本 bug 的核心缺口。
func newCapturingOllamaProvider() *mockllm.LLMProvider {
	// 名字含「本地」→ engine isLocalProvider 判定为本地；模型名 qwen3.5:9b → thinking 模型。
	return mockllm.NewLLMProvider("Ollama (本地)").AddResponse("你好，我是小蟹。").AddResponse("你好，我是小蟹。")
}

func runOllamaThinkingProbe(t *testing.T, thinkingPref string) *mockllm.LLMProvider {
	t.Helper()
	provider := newCapturingOllamaProvider()

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "Ollama (本地)"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama (本地)": {Model: "qwen3.5:9b", BaseURL: "http://localhost:11434/v1"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"Ollama (本地)": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	meta := map[string]string{}
	if thinkingPref != "" {
		meta["thinking"] = thinkingPref
	}
	msg := &adapter.Message{
		ID: "msg-ollama-think", Platform: adapter.PlatformAPI,
		UserID: "u-think", SessionID: "sess-think",
		Content:  "你好，一句话介绍你自己",
		Metadata: meta,
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}
	for range ch { // drain
	}
	return provider
}

func thinkingMetaReachingProvider(t *testing.T, p *mockllm.LLMProvider) (string, bool) {
	t.Helper()
	last := p.LastCall()
	if last == nil {
		t.Fatalf("provider 未收到任何请求")
	}
	if last.Metadata == nil {
		return "", false
	}
	v, ok := last.Metadata["thinking"]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// 默认（未开深度思考）：本地 thinking 模型必须显式 think:off（快、不夹带隐藏思考）。
func TestBug20260704_OllamaNativeThinking_DefaultOff(t *testing.T) {
	p := runOllamaThinkingProbe(t, "")
	got, present := thinkingMetaReachingProvider(t, p)
	if !present {
		t.Fatalf("本地 thinking 模型默认请求的 req.Metadata[\"thinking\"] 留空 —— Ollama 会进入慢的隐藏思考默认；必须显式 \"off\"")
	}
	if got != "off" {
		t.Fatalf("默认深度思考关闭时应显式 think:off，实际 = %q", got)
	}
}

// 开深度思考：必须显式 think:on，让原生 think:true 产出可显示的推理。
func TestBug20260704_OllamaNativeThinking_OnShowsReasoning(t *testing.T) {
	p := runOllamaThinkingProbe(t, "on")
	got, present := thinkingMetaReachingProvider(t, p)
	if !present || got != "on" {
		t.Fatalf("开启深度思考时应把 think:on 透传给本地 provider，实际 present=%v value=%q", present, got)
	}
}

// ★核心回归：深度思考开启时，本地 thinking 模型的推理必须**流式透出**（reasoning chunk），
// 而不是被「切换为有界非流式补全」丢掉。这是「不显示推理」的根因契约。
func TestBug20260704_OllamaThinkingOn_StreamsReasoning(t *testing.T) {
	provider := &reasoningStreamProvider{name: "Ollama (本地)"}

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "Ollama (本地)"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama (本地)": {Model: "qwen3.5:9b", BaseURL: "http://localhost:11434/v1"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"Ollama (本地)": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID: "msg-think-stream", Platform: adapter.PlatformAPI,
		UserID: "u-think", SessionID: "sess-think-stream",
		Content:  "42 的平方根约等于几",
		Metadata: map[string]string{"thinking": "on"},
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}
	var sawReasoning, sawContent bool
	for chunk := range ch {
		if strings.TrimSpace(chunk.Reasoning) != "" {
			sawReasoning = true
		}
		if strings.TrimSpace(chunk.Content) != "" {
			sawContent = true
		}
	}
	if !sawContent {
		t.Fatalf("应至少流式透出正文")
	}
	if !sawReasoning {
		t.Fatalf("深度思考开启时本地模型的推理必须流式透出（reasoning chunk），实际一条都没有 —— "+
			"当前 stream_calls=%d complete_calls=%d（若 complete>0/stream=0，说明被切到了丢弃推理的有界非流式补全）",
			provider.streamCalls, provider.completeCalls)
	}
}
