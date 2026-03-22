package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/agents"
	"github.com/hexagon-codes/hexclaw/cache"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// ReActEngine 基于 Hexagon ReAct Agent 的引擎实现
//
// 处理流程：
//  1. 接收统一消息
//  2. 快速路径: 匹配 Skill 直接执行
//  3. 主路径: 构建上下文 → ReAct Agent 推理+行动 → 返回结果
//  4. 保存消息历史
//
// 引擎在内部为每个请求创建临时 Agent 实例，
// 注入会话上下文和可用工具。
type ReActEngine struct {
	mu          sync.RWMutex
	cfg         *config.Config
	router      *llmrouter.Selector
	agentRouter *agentrouter.Dispatcher // 多 Agent 路由器（可为 nil）
	sessions    *session.Manager
	skills      *skill.DefaultRegistry
	store       storage.Store
	cache       *cache.Cache
	kb          *knowledge.Manager // 知识库管理器（可为 nil）
	factory     *agents.Factory    // Agent 角色工厂
	started     bool
	startAt     time.Time
	// 由技能市场安装/卸载同步维护：仅这些名称允许 Unregister，避免误删内置 Skill
	mpTracked map[string]struct{}
}

type modelOverrideProvider struct {
	inner hexagon.Provider
	model string
}

func (p *modelOverrideProvider) Name() string { return p.inner.Name() }

func (p *modelOverrideProvider) Complete(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	req.Model = p.model
	return p.inner.Complete(ctx, req)
}

func (p *modelOverrideProvider) Stream(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	req.Model = p.model
	return p.inner.Stream(ctx, req)
}

func (p *modelOverrideProvider) Models() []llm.ModelInfo {
	return p.inner.Models()
}

func (p *modelOverrideProvider) CountTokens(messages []llm.Message) (int, error) {
	return p.inner.CountTokens(messages)
}

type llmSelection struct {
	provider         hexagon.Provider
	providerName     string
	modelName        string
	explicitProvider bool
}

func llmCacheOptions(cfg config.LLMConfig) cache.Options {
	cacheTTL := 24 * time.Hour
	if cfg.Cache.TTL != "" {
		if d, err := time.ParseDuration(cfg.Cache.TTL); err == nil {
			cacheTTL = d
		}
	}
	maxEntries := cfg.Cache.MaxEntries
	if maxEntries == 0 {
		maxEntries = 10000
	}
	return cache.Options{
		Enabled:    cfg.Cache.Enabled,
		TTL:        cacheTTL,
		MaxEntries: maxEntries,
	}
}

func cloneLLMConfig(cfg config.LLMConfig) config.LLMConfig {
	cloned := cfg
	cloned.Providers = make(map[string]config.LLMProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		cloned.Providers[name] = provider
	}
	return cloned
}

// NewReActEngine 创建 ReAct 引擎
func NewReActEngine(
	cfg *config.Config,
	router *llmrouter.Selector,
	store storage.Store,
	skills *skill.DefaultRegistry,
) *ReActEngine {
	llmCache := cache.New(llmCacheOptions(cfg.LLM))

	return &ReActEngine{
		cfg:      cfg,
		router:   router,
		sessions: session.NewManager(store, cfg.Memory),
		skills:   skills,
		store:    store,
		cache:    llmCache,
		factory:  agents.NewFactory(),
	}
}

// ActiveLLMConfig 返回当前已经生效的 LLM 配置快照。
func (e *ReActEngine) ActiveLLMConfig() config.LLMConfig {
	e.mu.RLock()
	router := e.router
	cfg := cloneLLMConfig(e.cfg.LLM)
	e.mu.RUnlock()

	if router != nil {
		return router.ActiveConfig()
	}
	return cfg
}

// ReloadLLMConfig 原地热更新 LLM 路由与缓存配置。
func (e *ReActEngine) ReloadLLMConfig(_ context.Context, llmCfg config.LLMConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.router == nil {
		e.router = llmrouter.NewWithProviders(llmCfg, map[string]hexagon.Provider{})
	}
	if err := e.router.Reload(llmCfg); err != nil {
		return err
	}
	e.cache.Reconfigure(llmCacheOptions(llmCfg))
	e.cfg.LLM = cloneLLMConfig(llmCfg)
	return nil
}

// SetKnowledgeBase 设置知识库管理器
//
// 设置后，引擎在处理消息时会自动检索知识库，
// 将相关内容作为上下文注入 Agent。
func (e *ReActEngine) SetKnowledgeBase(kb *knowledge.Manager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.kb = kb
}

// KnowledgeBase 获取知识库管理器
func (e *ReActEngine) KnowledgeBase() *knowledge.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.kb
}

// SetAgentRouter 设置多 Agent 路由器
//
// 设置后，引擎在处理消息时会根据路由规则选择 Agent 配置（Provider/Model/SystemPrompt）。
func (e *ReActEngine) SetAgentRouter(r *agentrouter.Dispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.agentRouter = r
}

// AgentFactory 获取 Agent 角色工厂
func (e *ReActEngine) AgentFactory() *agents.Factory {
	return e.factory
}

// SetSkillEnabled 设置技能的运行时启用状态。
func (e *ReActEngine) SetSkillEnabled(name string, enabled bool) error {
	e.mu.RLock()
	skills := e.skills
	e.mu.RUnlock()
	if skills == nil {
		return fmt.Errorf("skill registry 未设置")
	}
	return skills.SetEnabled(name, enabled)
}

// SkillEnabled 返回技能是否在运行时生效。
func (e *ReActEngine) SkillEnabled(name string) (bool, bool) {
	e.mu.RLock()
	skills := e.skills
	e.mu.RUnlock()
	if skills == nil {
		return false, false
	}
	return skills.IsEnabled(name)
}

// Start 启动引擎
func (e *ReActEngine) Start(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = true
	e.startAt = time.Now()
	// 启动日志由 main 统一输出
	return nil
}

// Stop 停止引擎
func (e *ReActEngine) Stop(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = false
	log.Println("Agent 引擎已停止")
	return nil
}

// Health 健康检查
func (e *ReActEngine) Health(_ context.Context) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.started {
		return fmt.Errorf("引擎未启动")
	}
	return nil
}

// Process 同步处理消息
//
// 完整处理流程：
//  1. 获取或创建会话
//  2. 保存用户消息
//  3. 尝试快速路径（Skill Match）
//  4. 构建对话上下文
//  5. 使用 ReAct Agent 处理
//  6. 保存助手回复
//  7. 返回回复
func (e *ReActEngine) Process(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
	if err := validateIncomingMessage(msg); err != nil {
		return nil, err
	}

	// 1. 获取或创建会话
	sess, err := e.sessions.GetOrCreate(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("会话管理失败: %w", err)
	}
	msg.SessionID = sess.ID

	// 2. 尝试快速路径: Skill 关键词匹配
	if matched, ok := e.skills.Match(msg); ok {
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		skillArgs := map[string]any{
			"query":   msg.Content,
			"user_id": msg.UserID,
		}
		result, err := matched.Execute(ctx, skillArgs)
		if err != nil {
			return nil, fmt.Errorf("skill %s 执行失败: %w", matched.Name(), err)
		}

		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, result.Content); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		} else {
			assistantMessageID = record.ID
		}

		argsJSON, _ := json.Marshal(skillArgs)
		return &adapter.Reply{
			Content:  result.Content,
			Metadata: withAssistantMessageID(result.Metadata, assistantMessageID),
			ToolCalls: []adapter.ToolCall{{
				ID:        "tc-" + idgen.ShortID(),
				Name:      matched.Name(),
				Arguments: string(argsJSON),
				Result:    truncateResult(result.Content, 500),
			}},
		}, nil
	}

	cacheInput := adapter.AttachmentCacheKey(msg.Content, msg.Attachments)
	selection, err := e.resolveLLMSelection(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("llm 路由失败: %w", err)
	}

	// 3. 语义缓存查询
	if cached, ok := e.cache.Get(cacheInput, selection.providerName, selection.modelName); ok {
		log.Printf("语义缓存命中: %s", msg.Content[:min(20, len(msg.Content))])
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, cached); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		} else {
			assistantMessageID = record.ID
		}
		return &adapter.Reply{
			Content: cached,
			Metadata: withAssistantMessageID(map[string]string{
				"source":   "cache",
				"provider": selection.providerName,
				"model":    selection.modelName,
			}, assistantMessageID),
		}, nil
	}

	// 4. 主路径: 构建对话上下文（在 SaveUserMessage 之前，避免 history 重复包含当前消息）
	history, err := e.sessions.BuildContext(ctx, sess.ID)
	if err != nil {
		log.Printf("构建上下文失败: %v", err)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
		log.Printf("保存用户消息失败: %v", err)
	}

	// 5.5 知识库检索（RAG 上下文增强）
	var kbContext string
	if e.kb != nil && e.cfg.Knowledge.Enabled {
		topK := e.cfg.Knowledge.TopK
		if topK <= 0 {
			topK = 3
		}
		kbResult, kbErr := e.kb.Query(ctx, msg.Content, topK)
		if kbErr != nil {
			log.Printf("知识库检索失败: %v", kbErr)
		} else if kbResult != "" {
			kbContext = kbResult
			log.Printf("知识库命中: 查询=%s", msg.Content[:min(20, len(msg.Content))])
		}
	}

	if shouldUseDirectCompletion(history, kbContext, msg.Attachments) {
		return e.completeDirect(
			ctx,
			sess.ID,
			msg,
			history,
			kbContext,
			selection.provider,
			selection.providerName,
			selection.modelName,
			selection.explicitProvider,
			cacheInput,
		)
	}

	// 7. 创建 Agent（支持角色选择）
	roleName := msg.Metadata["role"] // 从消息元数据中获取角色
	reactAgent := e.createAgent(roleName, selection.provider, msg.Metadata)

	// 构建 Agent 输入
	agentInput := hexagon.Input{
		Query: msg.Content,
		Context: map[string]any{
			"session_id": sess.ID,
			"user_id":    msg.UserID,
			"platform":   string(msg.Platform),
			"provider":   selection.providerName,
		},
	}

	// 如果有历史消息，放入上下文
	if len(history) > 0 {
		agentInput.Context["history"] = history
	}

	// 如果有知识库检索结果，注入上下文
	if kbContext != "" {
		agentInput.Context["knowledge"] = kbContext
	}

	output, err := reactAgent.Run(ctx, agentInput)
	if err != nil {
		if selection.explicitProvider {
			return nil, fmt.Errorf("provider %s 调用失败: %w", selection.providerName, err)
		}
		// 尝试降级到备用 Provider
		fallbackP, fbName, fbErr := e.router.Fallback(selection.providerName)
		if fbErr != nil {
			return nil, fmt.Errorf("agent 执行失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 失败，降级到 %s: %v", selection.providerName, fbName, err)

		reactAgent = e.createAgent(roleName, fallbackP, msg.Metadata)
		output, err = reactAgent.Run(ctx, agentInput)
		if err != nil {
			return nil, fmt.Errorf("agent 执行失败（降级后）: %w", err)
		}
		selection.providerName = fbName
		selection.modelName = e.getProviderModel(fbName, msg.Metadata)
	}

	// 7. 保存助手回复
	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, output.Content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	} else {
		assistantMessageID = record.ID
	}

	// 8. 写入语义缓存（安全获取 model 名称，避免 map 访问空值）
	e.cache.Put(cacheInput, output.Content, selection.providerName, selection.modelName)

	// 9. 记录 Token 使用（用于成本控制）
	if output.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:        "cost-" + idgen.ShortID(),
			UserID:    msg.UserID,
			Provider:  selection.providerName,
			Model:     selection.modelName,
			Tokens:    output.Usage.TotalTokens,
			CreatedAt: time.Now(),
		}
		if err := e.store.SaveCost(ctx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
	}

	// 构建 Usage 信息
	var usage *adapter.Usage
	if output.Usage.TotalTokens > 0 {
		usage = &adapter.Usage{
			InputTokens:  output.Usage.PromptTokens,
			OutputTokens: output.Usage.CompletionTokens,
			TotalTokens:  output.Usage.TotalTokens,
			Provider:     selection.providerName,
			Model:        selection.modelName,
		}
	}

	replyMeta := buildReplyMetadata(msg.Metadata, selection.providerName, selection.modelName, assistantMessageID)

	var toolCalls []adapter.ToolCall
	for _, tc := range output.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		toolCalls = append(toolCalls, adapter.ToolCall{
			ID:        "tc-" + idgen.ShortID(),
			Name:      tc.Name,
			Arguments: string(argsJSON),
			Result:    truncateResult(tc.Result.String(), 500),
		})
	}

	return &adapter.Reply{
		Content:   output.Content,
		Metadata:  replyMeta,
		Usage:     usage,
		ToolCalls: toolCalls,
	}, nil
}

// createAgent 创建 Agent 实例
//
// 优先级: 角色名 > Agent 路由注入的 system prompt > 默认 prompt
func (e *ReActEngine) createAgent(roleName string, provider hexagon.Provider, metadata map[string]string) hexagon.Agent {
	if roleName != "" {
		agent, err := e.factory.CreateAgent(roleName, provider)
		if err != nil {
			log.Printf("创建角色 Agent 失败: %v，降级到默认", err)
		} else {
			return agent
		}
	}

	prompt := systemPrompt
	if metadata != nil && metadata["agent_prompt"] != "" {
		prompt = metadata["agent_prompt"]
	}

	return hexagon.NewReActAgent(
		hexagon.AgentWithName("hexclaw"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithSystemPrompt(prompt),
		hexagon.AgentWithMaxIterations(10),
	)
}

// getProviderModel 安全获取 Provider 的模型名称
func (e *ReActEngine) getProviderModel(providerName string, metadata map[string]string) string {
	if model := requestedModel(metadata); model != "" {
		return model
	}
	e.mu.RLock()
	router := e.router
	e.mu.RUnlock()
	if router != nil {
		if model := router.ProviderModel(providerName); model != "" {
			return model
		}
	}
	return providerName // 回退到 Provider 名称本身
}

// ProcessStream 流式处理消息
//
// 使用 LLM Provider 的原生 Stream 接口实现逐 token 输出。
// 流程与 Process 相同（会话/缓存/知识库/历史），但最终调用
// provider.Stream() 而非 agent.Run()，以实现打字机效果。
//
// 对于快速路径（Skill/缓存命中）降级为单 chunk 输出。
func (e *ReActEngine) ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	if err := validateIncomingMessage(msg); err != nil {
		return nil, err
	}

	// 1. 获取或创建会话
	sess, err := e.sessions.GetOrCreate(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("会话管理失败: %w", err)
	}
	msg.SessionID = sess.ID

	// 2. 尝试快速路径: Skill 匹配 → 单 chunk 返回
	if matched, ok := e.skills.Match(msg); ok {
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		skillArgs := map[string]any{
			"query":   msg.Content,
			"user_id": msg.UserID,
		}
		result, err := matched.Execute(ctx, skillArgs)
		if err != nil {
			return nil, fmt.Errorf("skill %s 执行失败: %w", matched.Name(), err)
		}
		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, result.Content); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		} else {
			assistantMessageID = record.ID
		}
		argsJSON, _ := json.Marshal(skillArgs)
		tc := []adapter.ToolCall{{
			ID:        "tc-" + idgen.ShortID(),
			Name:      matched.Name(),
			Arguments: string(argsJSON),
			Result:    truncateResult(result.Content, 500),
		}}
		return singleChunkWithTools(result.Content, withAssistantMessageID(result.Metadata, assistantMessageID), tc), nil
	}

	cacheInput := adapter.AttachmentCacheKey(msg.Content, msg.Attachments)
	selection, err := e.resolveLLMSelection(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("llm 路由失败: %w", err)
	}

	// 3. 语义缓存命中 → 单 chunk 返回
	if cached, ok := e.cache.Get(cacheInput, selection.providerName, selection.modelName); ok {
		log.Printf("语义缓存命中: %s", msg.Content[:min(20, len(msg.Content))])
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, cached); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		} else {
			assistantMessageID = record.ID
		}
		return singleChunk(cached, withAssistantMessageID(map[string]string{
			"source":   "cache",
			"provider": selection.providerName,
			"model":    selection.modelName,
		}, assistantMessageID)), nil
	}

	// 4. 构建对话上下文（在保存用户消息之前，避免 history 中重复包含当前消息）
	history, err := e.sessions.BuildContext(ctx, sess.ID)
	if err != nil {
		log.Printf("构建上下文失败: %v", err)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
		log.Printf("保存用户消息失败: %v", err)
	}

	// 5.5 知识库检索（RAG）
	var kbContext string
	if e.kb != nil && e.cfg.Knowledge.Enabled {
		topK := e.cfg.Knowledge.TopK
		if topK <= 0 {
			topK = 3
		}
		kbResult, kbErr := e.kb.Query(ctx, msg.Content, topK)
		if kbErr != nil {
			log.Printf("知识库检索失败: %v", kbErr)
		} else if kbResult != "" {
			kbContext = kbResult
			log.Printf("知识库命中: 查询=%s", msg.Content[:min(20, len(msg.Content))])
		}
	}

	// 7. 构建 CompletionRequest（含 system prompt + 历史 + 知识库 + 用户消息）
	req := e.buildCompletionRequest(msg, history, kbContext)

	// 8. 调用 provider.Stream()
	llmStream, err := selection.provider.Stream(ctx, req)
	if err != nil {
		if selection.explicitProvider {
			return nil, fmt.Errorf("provider %s 流式调用失败: %w", selection.providerName, err)
		}
		// 降级到备用 Provider
		fallbackP, fbName, fbErr := e.router.Fallback(selection.providerName)
		if fbErr != nil {
			return nil, fmt.Errorf("流式调用失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 流式调用失败，降级到 %s: %v", selection.providerName, fbName, err)
		llmStream, err = fallbackP.Stream(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("流式调用失败（降级后）: %w", err)
		}
		selection.providerName = fbName
		selection.modelName = e.getProviderModel(fbName, msg.Metadata)
	}

	// 9. 启动 goroutine 将 LLMStreamChunk 转发为 adapter.ReplyChunk
	ch := make(chan *adapter.ReplyChunk, 16)
	go e.pipeStream(ctx, ch, llmStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput)

	return ch, nil
}

// pipeStream 将 LLM 流式响应转发到适配器 channel，流结束后保存回复/缓存/成本
func (e *ReActEngine) pipeStream(
	ctx context.Context,
	ch chan<- *adapter.ReplyChunk,
	llmStream *hexagon.LLMStream,
	sessionID string,
	msg *adapter.Message,
	providerName string,
	modelName string,
	cacheInput string,
) {
	defer close(ch)
	defer llmStream.Close()

	var fullContent strings.Builder

	for chunk := range llmStream.Chunks() {
		if chunk.Content == "" {
			continue
		}
		fullContent.WriteString(chunk.Content)

		select {
		case ch <- &adapter.ReplyChunk{Content: chunk.Content}:
		case <-ctx.Done():
			ch <- &adapter.ReplyChunk{Error: ctx.Err(), Done: true}
			return
		}
	}

	// 获取最终结果（含 Usage 统计）
	result := llmStream.Result()

	content := fullContent.String()

	// 使用独立 context 进行后续操作，避免请求 ctx 取消后无法保存
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	// 保存助手回复
	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(saveCtx, sessionID, content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	} else {
		assistantMessageID = record.ID
	}

	// 写入语义缓存
	e.cache.Put(cacheInput, content, providerName, modelName)

	// 发送结束标记（携带 Usage 和元数据）
	doneChunk := &adapter.ReplyChunk{
		Done:     true,
		Metadata: buildReplyMetadata(msg.Metadata, providerName, modelName, assistantMessageID),
	}
	if result != nil && result.Usage.TotalTokens > 0 {
		doneChunk.Usage = &adapter.Usage{
			InputTokens:  result.Usage.PromptTokens,
			OutputTokens: result.Usage.CompletionTokens,
			TotalTokens:  result.Usage.TotalTokens,
			Provider:     providerName,
			Model:        modelName,
		}
	}
	ch <- doneChunk

	// 记录 Token 使用
	if result != nil && result.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:        "cost-" + idgen.ShortID(),
			UserID:    msg.UserID,
			Provider:  providerName,
			Model:     modelName,
			Tokens:    result.Usage.TotalTokens,
			CreatedAt: time.Now(),
		}
		if err := e.store.SaveCost(saveCtx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
	}
}

// buildStreamMessages 构建流式请求的消息列表
//
// 当 attachments 包含图片时，用户消息会构建为 MultiContent 格式（文本 + image_url），
// 底层 ai-core Provider 会自动识别并发送为多模态 API 请求。
func (e *ReActEngine) buildStreamMessages(roleName string, history []hexagon.Message, kbContext, userQuery string, metadata map[string]string, attachments []adapter.Attachment) []hexagon.Message {
	var messages []hexagon.Message

	// System prompt 优先级: 角色名 > Agent 路由注入 > 默认
	sysContent := systemPrompt
	if roleName != "" {
		if role, ok := e.factory.GetRole(roleName); ok {
			sysContent = role.ToSystemPrompt()
		}
	} else if metadata != nil && metadata["agent_prompt"] != "" {
		sysContent = metadata["agent_prompt"]
	}
	if kbContext != "" {
		sysContent += "\n\n[参考知识]\n" + kbContext
	}
	messages = append(messages, hexagon.Message{
		Role:    "system",
		Content: sysContent,
	})

	// 历史消息
	messages = append(messages, history...)

	// 当前用户消息（支持多模态附件）
	messages = append(messages, adapter.BuildUserMessage(userQuery, attachments))

	return messages
}

func (e *ReActEngine) buildCompletionRequest(msg *adapter.Message, history []hexagon.Message, kbContext string) hexagon.CompletionRequest {
	req := hexagon.CompletionRequest{
		Messages: e.buildStreamMessages(msg.Metadata["role"], history, kbContext, msg.Content, msg.Metadata, msg.Attachments),
	}
	applyCompletionOverrides(&req, msg.Metadata)
	return req
}

func (e *ReActEngine) completeDirect(
	ctx context.Context,
	sessionID string,
	msg *adapter.Message,
	history []hexagon.Message,
	kbContext string,
	provider hexagon.Provider,
	providerName string,
	modelName string,
	explicitProvider bool,
	cacheInput string,
) (*adapter.Reply, error) {
	req := e.buildCompletionRequest(msg, history, kbContext)
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		if explicitProvider {
			return nil, fmt.Errorf("provider %s 调用失败: %w", providerName, err)
		}
		fallbackP, fbName, fbErr := e.router.Fallback(providerName)
		if fbErr != nil {
			return nil, fmt.Errorf("多模态补全失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 多模态补全失败，降级到 %s: %v", providerName, fbName, err)
		resp, err = fallbackP.Complete(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("多模态补全失败（降级后）: %w", err)
		}
		providerName = fbName
		modelName = e.getProviderModel(fbName, msg.Metadata)
	}

	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sessionID, resp.Content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	} else {
		assistantMessageID = record.ID
	}

	e.cache.Put(cacheInput, resp.Content, providerName, modelName)

	if resp.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:        "cost-" + idgen.ShortID(),
			UserID:    msg.UserID,
			Provider:  providerName,
			Model:     modelName,
			Tokens:    resp.Usage.TotalTokens,
			CreatedAt: time.Now(),
		}
		if err := e.store.SaveCost(ctx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
	}

	return &adapter.Reply{
		Content:   resp.Content,
		Metadata:  buildReplyMetadata(msg.Metadata, providerName, modelName, assistantMessageID),
		Usage:     buildUsage(resp.Usage, providerName, modelName),
		ToolCalls: translateProviderToolCalls(resp.ToolCalls),
	}, nil
}

func shouldUseDirectCompletion(history []hexagon.Message, kbContext string, attachments []adapter.Attachment) bool {
	if len(history) > 0 || kbContext != "" {
		return true
	}
	if len(adapter.FilterImageAttachments(attachments)) > 0 {
		return true
	}
	for _, msg := range history {
		if msg.HasMultiContent() {
			return true
		}
	}
	return false
}

func applyCompletionOverrides(req *hexagon.CompletionRequest, metadata map[string]string) {
	if metadata == nil {
		return
	}
	if model := requestedModel(metadata); model != "" {
		req.Model = model
	}
	if raw := metadata["agent_max_tokens"]; raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			req.MaxTokens = n
		}
	}
	if raw := metadata["agent_temperature"]; raw != "" {
		if temperature, err := strconv.ParseFloat(raw, 64); err == nil {
			req.Temperature = &temperature
		}
	}
}

func buildUsage(usage hexagon.Usage, providerName, modelName string) *adapter.Usage {
	if usage.TotalTokens == 0 {
		return nil
	}
	return &adapter.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
		Provider:     providerName,
		Model:        modelName,
	}
}

func buildReplyMetadata(metadata map[string]string, providerName, modelName, assistantMessageID string) map[string]string {
	replyMeta := map[string]string{
		"provider": providerName,
		"model":    modelName,
	}
	if metadata == nil {
		return withAssistantMessageID(replyMeta, assistantMessageID)
	}
	if v := metadata["route_source"]; v != "" {
		replyMeta["route_source"] = v
	}
	if v := metadata["routed_agent"]; v != "" {
		replyMeta["routed_agent"] = v
	}
	return withAssistantMessageID(replyMeta, assistantMessageID)
}

func withAssistantMessageID(metadata map[string]string, assistantMessageID string) map[string]string {
	if assistantMessageID == "" {
		return metadata
	}

	merged := make(map[string]string, len(metadata)+1)
	for key, value := range metadata {
		merged[key] = value
	}
	merged["backend_message_id"] = assistantMessageID
	return merged
}

func translateProviderToolCalls(toolCalls []streamx.ToolCall) []adapter.ToolCall {
	if len(toolCalls) == 0 {
		return nil
	}
	result := make([]adapter.ToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		result = append(result, adapter.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	return result
}

func validateIncomingMessage(msg *adapter.Message) error {
	if !adapter.HasMessageInput(msg.Content, msg.Attachments) {
		return fmt.Errorf("message 不能为空")
	}
	if err := adapter.ValidateAttachments(msg.Attachments); err != nil {
		return fmt.Errorf("附件校验失败: %w", err)
	}
	return nil
}

// singleChunk 将完整内容包装为单 chunk channel（用于快速路径）
func singleChunk(content string, metadata map[string]string) <-chan *adapter.ReplyChunk {
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Content: content, Done: true, Metadata: metadata}
	close(ch)
	return ch
}

func singleChunkWithTools(content string, metadata map[string]string, toolCalls []adapter.ToolCall) <-chan *adapter.ReplyChunk {
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Content: content, Done: true, Metadata: metadata, ToolCalls: toolCalls}
	close(ch)
	return ch
}

// resolveProvider 根据请求的 provider 名称解析 LLM Provider
//
// 如果 providerHint 为空或 "auto"，使用路由器默认策略选择；
// 否则尝试使用指定的 Provider，不存在则直接报错。
func (e *ReActEngine) resolveProvider(ctx context.Context, providerHint string, msg *adapter.Message) (hexagon.Provider, string, error) {
	e.mu.RLock()
	router := e.router
	e.mu.RUnlock()
	if router == nil {
		return nil, "", fmt.Errorf("没有可用的 LLM Provider")
	}

	// 优先级: 显式指定 > Agent 路由 > 默认路由
	hint := providerHint

	// 如果未显式指定 Provider，尝试通过 Agent 路由获取
	// 优先规则路由；规则未命中时尝试 LLM 语义分类（如已配置）
	if (hint == "" || hint == "auto") && e.agentRouter != nil && msg != nil {
		req := agentrouter.RouteRequest{
			Platform:   string(msg.Platform),
			InstanceID: msg.InstanceID,
			UserID:     msg.UserID,
			ChatID:     msg.ChatID,
		}
		result, routeSource := e.agentRouter.RouteWithFallback(ctx, req, msg.Content)
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]string)
		}
		msg.Metadata["route_source"] = string(routeSource)
		if result != nil && result.AgentConfig != nil {
			msg.Metadata["routed_agent"] = result.AgentName
			if result.AgentConfig.Provider != "" {
				hint = result.AgentConfig.Provider
			}
			if result.AgentConfig.Model != "" {
				msg.Metadata["agent_model"] = result.AgentConfig.Model
			}
			if result.AgentConfig.SystemPrompt != "" && msg.Metadata["role"] == "" {
				msg.Metadata["agent_prompt"] = result.AgentConfig.SystemPrompt
			}
			if result.AgentConfig.MaxTokens > 0 {
				msg.Metadata["agent_max_tokens"] = fmt.Sprintf("%d", result.AgentConfig.MaxTokens)
			}
			if result.AgentConfig.Temperature > 0 {
				msg.Metadata["agent_temperature"] = fmt.Sprintf("%.2f", result.AgentConfig.Temperature)
			}
		}
	}

	if hint == "" || hint == "auto" {
		return router.Route(ctx)
	}
	if p, ok := router.Get(hint); ok {
		return p, hint, nil
	}
	return nil, "", fmt.Errorf("指定的 provider %q 不存在", hint)
}

func (e *ReActEngine) resolveLLMSelection(ctx context.Context, msg *adapter.Message) (llmSelection, error) {
	providerHint := requestedProvider(msg.Metadata)
	provider, providerName, err := e.resolveProvider(ctx, providerHint, msg)
	if err != nil {
		return llmSelection{}, err
	}

	modelName := e.getProviderModel(providerName, msg.Metadata)
	if modelName != "" {
		provider = &modelOverrideProvider{inner: provider, model: modelName}
	}

	return llmSelection{
		provider:         provider,
		providerName:     providerName,
		modelName:        modelName,
		explicitProvider: providerHint != "",
	}, nil
}

func requestedProvider(metadata map[string]string) string {
	if metadata == nil {
		return ""
	}
	provider := strings.TrimSpace(metadata["provider"])
	if strings.EqualFold(provider, "auto") {
		return ""
	}
	return provider
}

func requestedModel(metadata map[string]string) string {
	if metadata == nil {
		return ""
	}
	if model := strings.TrimSpace(metadata["model"]); model != "" && !strings.EqualFold(model, "auto") {
		return model
	}
	return strings.TrimSpace(metadata["agent_model"])
}

// systemPrompt HexClaw 系统提示词
const systemPrompt = `你是「小蟹」🦀，HexClaw 的 AI 助手。

关于你：
- 名字叫「小蟹」，用户也可以叫你"河蟹"、"HexClaw"
- 由 Hexagon AI Agent Engine 驱动
- 本地部署，数据私有：API Key 直连模型服务商，中间零代理
- 原生支持 MCP 工具协议：文件、数据库、API 即插即用
- 当用户问"你是谁"时，介绍自己是「小蟹」，不要提及底层 LLM 模型名称

性格：
- 友好、专业、略带幽默感，偶尔横行一下 🦀
- 回答简洁直接，不拖泥带水
- 诚实可靠：不确定的事情坦诚告知，不编造信息
- 用中文回答，除非用户明确要求使用其他语言

能力：
- 智能编排：多步骤任务自动执行
- 本地操控：直接操作本地文件
- 代码生成：自动化开发任务
- 知识问答：基于个人知识库 RAG 增强检索
- 工具调用：天气查询、网络搜索、翻译等内置技能
- MCP 扩展：通过 Model Context Protocol 接入任意外部工具`

func truncateResult(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
