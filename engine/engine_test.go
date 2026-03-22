package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestReActEngine_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	skills := skill.NewRegistry()

	// 没有 LLM Provider 时无法创建路由器，但可以测试引擎生命周期
	// 使用一个假的 API Key 创建路由器（不实际调用）
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	eng := NewReActEngine(cfg, router, store, skills)

	// 启动前健康检查应失败
	ctx := context.Background()
	if err := eng.Health(ctx); err == nil {
		t.Error("启动前健康检查应失败")
	}

	// 启动
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 启动后健康检查应通过
	if err := eng.Health(ctx); err != nil {
		t.Errorf("启动后健康检查应通过: %v", err)
	}

	// 停止
	if err := eng.Stop(ctx); err != nil {
		t.Fatalf("停止失败: %v", err)
	}

	// 停止后健康检查应失败
	if err := eng.Health(ctx); err == nil {
		t.Error("停止后健康检查应失败")
	}
}

func TestReActEngine_SkillFastPath(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)

	// 注册一个模拟 Skill
	skills := skill.NewRegistry()
	skills.Register(&echoSkill{})

	eng := NewReActEngine(cfg, router, store, skills)
	eng.Start(context.Background())
	defer eng.Stop(context.Background())

	// 触发快速路径
	msg := &adapter.Message{
		ID:       "msg-001",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "/echo hello world",
	}

	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	if reply.Content != "echo: hello world" {
		t.Errorf("期望 'echo: hello world'，得到 %q", reply.Content)
	}
	if reply.Metadata["backend_message_id"] == "" {
		t.Fatal("同步回复应携带 backend_message_id")
	}
}

func TestReActEngine_ProcessStream_SkillFastPath(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)

	skills := skill.NewRegistry()
	skills.Register(&echoSkill{})

	eng := NewReActEngine(cfg, router, store, skills)
	eng.Start(context.Background())
	defer eng.Stop(context.Background())

	msg := &adapter.Message{
		ID:       "msg-002",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "/echo streaming test",
	}

	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}

	var chunks []adapter.ReplyChunk
	for chunk := range ch {
		chunks = append(chunks, *chunk)
	}

	if len(chunks) != 1 {
		t.Fatalf("期望 1 个 chunk（快速路径），得到 %d", len(chunks))
	}
	if !chunks[0].Done {
		t.Error("快速路径 chunk 应标记 Done=true")
	}
	if chunks[0].Content != "echo: streaming test" {
		t.Errorf("期望 'echo: streaming test'，得到 %q", chunks[0].Content)
	}
	if chunks[0].Metadata["backend_message_id"] == "" {
		t.Fatal("流式快速路径应携带 backend_message_id")
	}
}

func TestSingleChunk(t *testing.T) {
	ch := singleChunk("hello", nil)
	var got []adapter.ReplyChunk
	for c := range ch {
		got = append(got, *c)
	}
	if len(got) != 1 || got[0].Content != "hello" || !got[0].Done {
		t.Errorf("singleChunk 结果不符合预期: %+v", got)
	}
}

func TestBuildStreamMessages(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	defer store.Close()
	store.Init(context.Background())
	skills := skill.NewRegistry()
	eng := NewReActEngine(cfg, router, store, skills)

	// 无历史、无知识库、无角色
	msgs := eng.buildStreamMessages("", nil, "", "你好", nil, nil)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息（system+user），得到 %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("第一条消息应为 system，得到 %q", msgs[0].Role)
	}
	if msgs[1].Content != "你好" {
		t.Errorf("用户消息内容不匹配: %q", msgs[1].Content)
	}

	// 有知识库上下文
	msgs = eng.buildStreamMessages("", nil, "相关知识内容", "你好", nil, nil)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "[参考知识]") {
		t.Error("system 消息应包含知识库内容")
	}

	// 有历史消息
	history := []hexagon.Message{
		{Role: "user", Content: "之前的问题"},
		{Role: "assistant", Content: "之前的回答"},
	}
	msgs = eng.buildStreamMessages("", history, "", "新问题", nil, nil)
	if len(msgs) != 4 {
		t.Fatalf("期望 4 条消息（system+2history+user），得到 %d", len(msgs))
	}
	if msgs[3].Content != "新问题" {
		t.Errorf("最后一条应为用户新消息: %q", msgs[3].Content)
	}

	// 有角色
	msgs = eng.buildStreamMessages("coder", nil, "", "写代码", nil, nil)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d", len(msgs))
	}
	// coder 角色的 system prompt 应不同于默认
	if msgs[0].Content == systemPrompt {
		t.Error("指定 coder 角色后 system prompt 应不同于默认")
	}

	msgs = eng.buildStreamMessages("", nil, "", "按路由执行", map[string]string{"agent_prompt": "custom prompt"}, nil)
	if msgs[0].Content != "custom prompt" {
		t.Fatalf("agent_prompt 未生效: %q", msgs[0].Content)
	}

	// 有图片附件 → MultiContent
	imgs := []adapter.Attachment{
		{Type: "image", Name: "test.png", Mime: "image/png", Data: "iVBOR"},
	}
	msgs = eng.buildStreamMessages("", nil, "", "描述图片", nil, imgs)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d", len(msgs))
	}
	if !msgs[1].HasMultiContent() {
		t.Fatal("附带图片的用户消息应使用 MultiContent")
	}
	if len(msgs[1].MultiContent) != 2 {
		t.Fatalf("期望 2 个 ContentPart（text+image），得到 %d", len(msgs[1].MultiContent))
	}
}

func TestReActEngine_ReloadLLMConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", Model: "gpt-4o"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"openai": nil,
	})

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	next := config.LLMConfig{
		Default: "智谱",
		Providers: map[string]config.LLMProviderConfig{
			"智谱": {APIKey: "sk-zhipu", Model: "glm-5"},
		},
		Cache: config.LLMCacheConfig{
			Enabled:    true,
			TTL:        "1h",
			MaxEntries: 128,
		},
	}

	if err := eng.ReloadLLMConfig(context.Background(), next); err != nil {
		t.Fatalf("ReloadLLMConfig 失败: %v", err)
	}

	active := eng.ActiveLLMConfig()
	if active.Default != "智谱" {
		t.Fatalf("期望默认 provider 为智谱，实际 %q", active.Default)
	}
	if _, ok := active.Providers["智谱"]; !ok {
		t.Fatalf("期望活跃配置包含智谱，实际 %+v", active.Providers)
	}
	if _, ok := active.Providers["openai"]; ok {
		t.Fatalf("旧 provider 不应保留: %+v", active.Providers)
	}
}

func TestReActEngine_ProcessUsesDirectCompletionForAttachments(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		if !last.HasMultiContent() {
			t.Fatal("同步附件请求应走多模态 Completion")
		}
		return &hexagon.CompletionResponse{
			Content: "vision reply",
			Usage:   hexagon.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}, nil
	})

	eng := newEngineWithProvider(t, provider)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-vision",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "描述图片",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "image-a"},
		},
	})
	if err != nil {
		t.Fatalf("Process 失败: %v", err)
	}
	if reply.Content != "vision reply" {
		t.Fatalf("回复内容不匹配: %q", reply.Content)
	}
	if provider.CallCount() != 1 {
		t.Fatalf("期望 provider 调用 1 次，实际 %d", provider.CallCount())
	}
}

func TestReActEngine_ProcessUsesSessionHistoryForTextFollowUp(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		switch last.Content {
		case "第一句":
			return &hexagon.CompletionResponse{
				Content: "reply-1",
				Usage:   hexagon.Usage{TotalTokens: 10},
			}, nil
		case "继续刚才的话题":
			if len(req.Messages) < 4 {
				t.Fatalf("follow-up 请求应包含历史消息，实际仅 %d 条", len(req.Messages))
			}
			if req.Messages[1].Content != "第一句" {
				t.Fatalf("历史用户消息未注入: %#v", req.Messages)
			}
			if req.Messages[2].Content != "reply-1" {
				t.Fatalf("历史助手消息未注入: %#v", req.Messages)
			}
			return &hexagon.CompletionResponse{
				Content: "reply-2",
				Usage:   hexagon.Usage{TotalTokens: 12},
			}, nil
		default:
			t.Fatalf("收到未预期的请求内容: %q", last.Content)
			return nil, nil
		}
	})

	eng := newEngineWithProvider(t, provider)

	firstMsg := &adapter.Message{
		ID:       "msg-text-1",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "第一句",
	}
	firstReply, err := eng.Process(context.Background(), firstMsg)
	if err != nil {
		t.Fatalf("首轮请求失败: %v", err)
	}
	if firstReply.Content != "reply-1" {
		t.Fatalf("首轮回复不匹配: %q", firstReply.Content)
	}

	secondReply, err := eng.Process(context.Background(), &adapter.Message{
		ID:        "msg-text-2",
		Platform:  adapter.PlatformAPI,
		UserID:    "user-001",
		SessionID: firstMsg.SessionID,
		Content:   "继续刚才的话题",
	})
	if err != nil {
		t.Fatalf("follow-up 请求失败: %v", err)
	}
	if secondReply.Content != "reply-2" {
		t.Fatalf("follow-up 回复不匹配: %q", secondReply.Content)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("期望 provider 调用 2 次，实际 %d", provider.CallCount())
	}
}

func TestReActEngine_ProcessCacheSeparatesAttachments(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		imageURL := last.MultiContent[len(last.MultiContent)-1].ImageURL.URL
		content := "reply-b"
		if strings.Contains(imageURL, "image-a") {
			content = "reply-a"
		}
		return &hexagon.CompletionResponse{
			Content: content,
			Usage:   hexagon.Usage{TotalTokens: 10},
		}, nil
	})

	eng := newEngineWithProvider(t, provider)

	replyA1, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-a1",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "这是什么",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "image-a"},
		},
	})
	if err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}
	replyB, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-b1",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "这是什么",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "image-b"},
		},
	})
	if err != nil {
		t.Fatalf("第二个请求失败: %v", err)
	}
	replyA2, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-a2",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "这是什么",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "image-a"},
		},
	})
	if err != nil {
		t.Fatalf("第三个请求失败: %v", err)
	}

	if replyA1.Content != "reply-a" || replyA2.Content != "reply-a" {
		t.Fatalf("图片 A 的回复不匹配: %q / %q", replyA1.Content, replyA2.Content)
	}
	if replyB.Content != "reply-b" {
		t.Fatalf("图片 B 的回复不匹配: %q", replyB.Content)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("缓存应只命中重复图片，期望 provider 调用 2 次，实际 %d", provider.CallCount())
	}
}

func TestReActEngine_ProcessUsesMultimodalHistory(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		if len(req.Messages) >= 3 {
			hasHistoryImage := false
			for _, message := range req.Messages[:len(req.Messages)-1] {
				if message.HasMultiContent() {
					hasHistoryImage = true
					break
				}
			}
			if !hasHistoryImage {
				t.Fatal("多轮 follow-up 应保留历史图片消息")
			}
		}
		return &hexagon.CompletionResponse{
			Content: "ok",
			Usage:   hexagon.Usage{TotalTokens: 8},
		}, nil
	})

	eng := newEngineWithProvider(t, provider)

	firstMsg := &adapter.Message{
		ID:       "msg-first",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "先看这张图",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "image-a"},
		},
	}
	firstReply, err := eng.Process(context.Background(), firstMsg)
	if err != nil {
		t.Fatalf("首轮请求失败: %v", err)
	}
	if firstReply == nil {
		t.Fatal("首轮请求回复为空")
	}

	_, err = eng.Process(context.Background(), &adapter.Message{
		ID:        "msg-follow",
		Platform:  adapter.PlatformAPI,
		UserID:    "user-001",
		SessionID: firstMsg.SessionID,
		Content:   "继续说明细节",
	})
	if err != nil {
		t.Fatalf("follow-up 请求失败: %v", err)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("期望 provider 调用 2 次，实际 %d", provider.CallCount())
	}
}

func TestReActEngine_ProcessHonorsExplicitProviderModelAndDisablesFallback(t *testing.T) {
	primary := mockllm.NewLLMProvider("zhipu").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		if req.Model != "glm-5" {
			t.Fatalf("显式模型未生效，实际 %q", req.Model)
		}
		return nil, context.DeadlineExceeded
	})
	fallback := mockllm.NewLLMProvider("qwen").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "fallback should not run"}, nil
	})

	eng := newEngineWithProviders(t, map[string]hexagon.Provider{
		"智谱": primary,
		"通义": fallback,
	}, map[string]config.LLMProviderConfig{
		"智谱": {Model: "glm-4"},
		"通义": {Model: "qwen-max"},
	}, "智谱")

	_, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-explicit",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "你好",
		Metadata: map[string]string{
			"provider": "智谱",
			"model":    "glm-5",
		},
	})
	if err == nil {
		t.Fatal("期望显式 provider 失败时直接返回错误")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("错误未透传底层原因: %v", err)
	}
	if fallback.CallCount() != 0 {
		t.Fatalf("显式 provider 失败时不应跨 provider fallback，实际调用 %d 次", fallback.CallCount())
	}
}

func TestReActEngine_ProcessStreamHonorsExplicitProviderModelAndDisablesFallback(t *testing.T) {
	primary := mockllm.NewLLMProvider("zhipu").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		if req.Model != "glm-5" {
			t.Fatalf("流式显式模型未生效，实际 %q", req.Model)
		}
		return nil, context.DeadlineExceeded
	})
	fallback := mockllm.NewLLMProvider("qwen").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "fallback should not run"}, nil
	})

	eng := newEngineWithProviders(t, map[string]hexagon.Provider{
		"智谱": primary,
		"通义": fallback,
	}, map[string]config.LLMProviderConfig{
		"智谱": {Model: "glm-4"},
		"通义": {Model: "qwen-max"},
	}, "智谱")

	_, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID:       "msg-explicit-stream",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "你好",
		Metadata: map[string]string{
			"provider": "智谱",
			"model":    "glm-5",
		},
	})
	if err == nil {
		t.Fatal("期望流式显式 provider 失败时直接返回错误")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("流式错误未透传底层原因: %v", err)
	}
	if fallback.CallCount() != 0 {
		t.Fatalf("流式显式 provider 失败时不应跨 provider fallback，实际调用 %d 次", fallback.CallCount())
	}
}

// echoSkill 测试用的 echo Skill
type echoSkill struct{}

func (s *echoSkill) Name() string        { return "echo" }
func (s *echoSkill) Description() string { return "回显输入" }
func (s *echoSkill) Match(content string) bool {
	return len(content) > 6 && content[:6] == "/echo "
}
func (s *echoSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query := args["query"].(string)
	return &skill.Result{Content: "echo: " + query[6:]}, nil
}

func newEngineWithProvider(t *testing.T, provider hexagon.Provider) *ReActEngine {
	t.Helper()

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
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"test": provider,
	})

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

func newEngineWithProviders(
	t *testing.T,
	providers map[string]hexagon.Provider,
	providerCfg map[string]config.LLMProviderConfig,
	defaultProvider string,
) *ReActEngine {
	t.Helper()

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
	cfg.LLM.Default = defaultProvider
	cfg.LLM.Providers = providerCfg
	router := llmrouter.NewWithProviders(cfg.LLM, providers)

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}
