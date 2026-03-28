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
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/agents"
	"github.com/hexagon-codes/hexclaw/cache"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/memory"
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
	compactor   *session.Compactor // 上下文压缩器
	fileMem     *memory.FileMemory    // 文件记忆系统（可为 nil）
	vectorMem   *memory.VectorMemory // 向量语义记忆（可为 nil）
	factory     *agents.Factory      // Agent 角色工厂
	started     bool
	startAt     time.Time
	// 由技能市场安装/卸载同步维护：仅这些名称允许 Unregister，避免误删内置 Skill
	mpTracked map[string]struct{}

	// D1-D2: 统一工具循环基础设施
	toolCollector *ToolCollector        // 工具收集器 (Skill + MCP)
	toolExecutor  *ToolExecutor         // 工具执行器 (含 Hook 链)
	sessionLock   *session.SessionLock  // 会话并发锁
	budgetCfg     *BudgetConfig         // D17: 预算配置 (非 nil 时每次请求创建独立 BudgetController)
	bgWg          sync.WaitGroup        // G3: 等待后台 goroutine (压缩/记忆) 完成
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

	eng := &ReActEngine{
		cfg:      cfg,
		router:   router,
		sessions: session.NewManager(store, cfg.Memory),
		skills:   skills,
		store:    store,
		cache:    llmCache,
		factory:  agents.NewFactory(),
	}
	if cfg.Compaction.Enabled {
		eng.compactor = session.NewCompactor(store, session.CompactionConfig{
			MaxMessages: cfg.Compaction.MaxMessages,
			KeepRecent:  cfg.Compaction.KeepRecent,
		})
	}
	return eng
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

// SetFileMemory 设置文件记忆系统，启用自动记忆提取。
func (e *ReActEngine) SetFileMemory(fm *memory.FileMemory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fileMem = fm
}

// SetVectorMemory 设置向量语义记忆
func (e *ReActEngine) SetVectorMemory(vm *memory.VectorMemory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vectorMem = vm
}

// GetVectorMemory 获取向量语义记忆
func (e *ReActEngine) GetVectorMemory() *memory.VectorMemory {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.vectorMem
}

// SetToolCollector 设置工具收集器
func (e *ReActEngine) SetToolCollector(tc *ToolCollector) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCollector = tc
}

// SetToolExecutor 设置工具执行器
func (e *ReActEngine) SetToolExecutor(te *ToolExecutor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolExecutor = te
}

// SetSessionLock 设置会话锁
func (e *ReActEngine) SetSessionLock(sl *session.SessionLock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessionLock = sl
}

// SetBudget 设置预算控制器
// SetBudget stores budget config; each request creates its own BudgetController (not shared).
func (e *ReActEngine) SetBudget(b *BudgetController) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Store config from the prototype; actual controllers are per-request
	e.budgetCfg = &BudgetConfig{
		MaxTokens:   b.maxTokens,
		MaxDuration: b.maxDuration,
		MaxCost:     b.maxCost,
	}
}

// LLMCache 返回 LLM 响应缓存实例，用于启动加载和关闭持久化。
func (e *ReActEngine) LLMCache() *cache.Cache {
	return e.cache
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
	// G3: 等待后台 goroutine（压缩/记忆提取）完成，防止 DB close 后写入
	e.bgWg.Wait()
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

	// 1.5 Session 锁: 串行化同一会话的并发请求 (对齐 OpenClaw Session Lane)
	if e.sessionLock != nil {
		unlock := e.sessionLock.Acquire(sess.ID)
		defer unlock()
	}

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

	// 6. 统一路径: completeWithTools (对齐 OpenClaw/Claude Code/OpenAI SDK)
	// 删除了 shouldUseDirectCompletion 分叉 — 始终传 tools，无工具调用时零额外延迟
	return e.completeWithTools(
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

// completeWithTools 统一工具循环
//
// 核心改动: 始终传 tools 给 LLM，如果 LLM 返回 tool_calls 则执行并继续循环。
// 对齐: OpenClaw single-path / OpenAI SDK Runner.run() / Claude Code Agent Loop
func (e *ReActEngine) completeWithTools(
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
	// Budget 控制: 有 BudgetConfig 时创建 per-request budget，否则硬限 5 轮
	const hardMaxTurns = 50 // Budget 模式下的绝对安全上限
	var budget *BudgetController
	e.mu.RLock()
	cfg := e.budgetCfg
	e.mu.RUnlock()
	if cfg != nil {
		budget = NewBudgetController(*cfg)
	}
	useBudget := budget != nil

	// 收集工具定义
	var tools []llm.ToolDefinition
	if e.toolCollector != nil {
		tools = e.toolCollector.Collect()
	}

	// 构建初始请求
	req := e.buildCompletionRequest(msg, history, kbContext)
	if len(tools) > 0 {
		req.Tools = tools
	}

	var allToolCalls []adapter.ToolCall
	messages := req.Messages

	for turn := 0; turn < hardMaxTurns; turn++ {
		// Budget 检查 (每轮开始前)
		if useBudget {
			if err := budget.Check(); err != nil {
				log.Printf("预算耗尽 (turn %d): %v", turn, err)
				break
			}
		} else if turn >= 5 {
			// 无 Budget 时硬限 5 轮
			break
		}

		req.Messages = messages
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			if explicitProvider || turn > 0 {
				return nil, fmt.Errorf("provider %s 调用失败: %w", providerName, err)
			}
			// 首轮可降级
			fallbackP, fbName, fbErr := e.router.Fallback(providerName)
			if fbErr != nil {
				return nil, fmt.Errorf("补全失败且无可用备用: %w", err)
			}
			log.Printf("Provider %s 失败，降级到 %s: %v", providerName, fbName, err)
			provider = fallbackP
			providerName = fbName
			modelName = e.getProviderModel(fbName, msg.Metadata)
			// 降级后需重新收集工具（不同 provider 可能支持不同数量）
			resp, err = provider.Complete(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("补全失败（降级后）: %w", err)
			}
		}

		// 记录 token 使用到 Budget
		if useBudget && resp.Usage.TotalTokens > 0 {
			budget.RecordTokens(resp.Usage.TotalTokens)
		}

		// 无 tool_calls → 最终回复 (最常见路径，零额外延迟)
		if !resp.HasToolCalls() {
			return e.finalizeReply(ctx, sessionID, msg, resp, providerName, modelName, cacheInput, allToolCalls)
		}

		// 有 tool_calls → 执行工具并追加到 messages
		// G2: 结构化 tool transcript — assistant 消息包含 ToolCalls 引用
		var toolCallRefs []llm.ToolCallRef
		for _, tc := range resp.ToolCalls {
			toolCallRefs = append(toolCallRefs, llm.ToolCallRef{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		messages = append(messages, llm.AssistantToolCallMessage(resp.Content, toolCallRefs))

		// 执行每个 tool_call 并追加结构化 tool result
		for _, tc := range resp.ToolCalls {
			var toolArgs map[string]any
			if tc.Arguments != "" {
				json.Unmarshal([]byte(tc.Arguments), &toolArgs)
			}

			var toolResult string
			if e.toolExecutor != nil {
				toolResult, err = e.toolExecutor.Execute(ctx, tc.Name, toolArgs)
				if err != nil {
					log.Printf("[tool] %s 执行失败: %v", tc.Name, err)
					toolResult = fmt.Sprintf("Error: tool %q execution failed", tc.Name)
				}
			} else {
				toolResult = "Error: tool executor not available"
			}

			// G2: 结构化 tool result — Role=tool + ToolCallID 关联
			messages = append(messages, llm.ToolResultMessage(tc.ID, toolResult))

			// 记录工具调用
			argsJSON, _ := json.Marshal(toolArgs)
			allToolCalls = append(allToolCalls, adapter.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: string(argsJSON),
				Result:    truncateResult(toolResult, 500),
			})
		}
	}

	// 超过上限（Budget 耗尽或硬限），返回最后一轮内容 + 警告
	if useBudget {
		log.Printf("预算耗尽，工具循环结束: %s", budget.Summary())
	} else {
		log.Printf("工具循环达到硬限 5 轮，强制返回")
	}
	lastResp, err := provider.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("最终补全失败: %w", err)
	}
	return e.finalizeReply(ctx, sessionID, msg, lastResp, providerName, modelName, cacheInput, allToolCalls)
}

// finalizeReply 完成回复的保存、缓存、成本记录等后处理
func (e *ReActEngine) finalizeReply(
	ctx context.Context,
	sessionID string,
	msg *adapter.Message,
	resp *llm.CompletionResponse,
	providerName, modelName, cacheInput string,
	toolCalls []adapter.ToolCall,
) (*adapter.Reply, error) {
	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sessionID, resp.Content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	} else {
		assistantMessageID = record.ID
	}

	e.cache.Put(cacheInput, resp.Content, providerName, modelName)

	if resp.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:               "cost-" + idgen.ShortID(),
			UserID:           msg.UserID,
			Provider:         providerName,
			Model:            modelName,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CreatedAt:        time.Now(),
		}
		if err := e.store.SaveCost(ctx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
		// Note: token 已在 completeWithTools 循环中按轮记录到 per-request budget
	}

	// 自动记忆提取（异步）
	e.autoExtractMemory(msg.Content, resp.Content)

	// 上下文压缩（异步，G3: 串行化后台写入）
	if e.compactor != nil {
		e.bgWg.Add(1)
		go func() {
			defer e.bgWg.Done()
			// 用独立 context，5 分钟超时防泄漏
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := e.compactor.CompactIfNeeded(bgCtx, sessionID, nil); err != nil {
				log.Printf("上下文压缩失败: %v", err)
			}
		}()
	}

	return &adapter.Reply{
		Content:   resp.Content,
		Metadata:  buildReplyMetadata(msg.Metadata, providerName, modelName, assistantMessageID),
		Usage:     buildUsage(resp.Usage, providerName, modelName),
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

	// 1.5 Session 锁 (注意: 流式路径在 goroutine 中释放锁)
	if e.sessionLock != nil {
		unlock := e.sessionLock.Acquire(sess.ID)
		// unlock 在 pipeStreamWithTools goroutine 结束时调用
		ctx = context.WithValue(ctx, "session_unlock", unlock)
	}

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

	// 7. 构建 CompletionRequest（含 tools + system prompt + 历史 + 知识库 + 用户消息）
	req := e.buildCompletionRequest(msg, history, kbContext)
	var tools []llm.ToolDefinition
	if e.toolCollector != nil {
		tools = e.toolCollector.Collect()
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	// 7.5 工具循环: 中间轮用 Complete (非流式)，最终轮用 Stream (流式)
	// G1: Budget 接入 ProcessStream — 有 Budget 时由 Budget 兜底，无 Budget 时硬限 5 轮
	const maxStreamToolTurns = 5
	var budget *BudgetController
	if e.budgetCfg != nil {
		budget = NewBudgetController(*e.budgetCfg)
	}
	useBudgetStream := budget != nil
	messages := req.Messages
	var allToolCalls []adapter.ToolCall

	for turn := 0; turn < maxStreamToolTurns; turn++ {
		if useBudgetStream {
			if err := budget.Check(); err != nil {
				log.Printf("[stream] 预算耗尽 (turn %d): %v", turn, err)
				break
			}
		}
		req.Messages = messages

		// 先用 Complete 探测是否有 tool_calls
		resp, err := selection.provider.Complete(ctx, req)
		if err != nil {
			if selection.explicitProvider || turn > 0 {
				return nil, fmt.Errorf("provider %s 调用失败: %w", selection.providerName, err)
			}
			fallbackP, fbName, fbErr := e.router.Fallback(selection.providerName)
			if fbErr != nil {
				return nil, fmt.Errorf("调用失败且无可用备用: %w", err)
			}
			log.Printf("Provider %s 失败，降级到 %s: %v", selection.providerName, fbName, err)
			selection.provider = fallbackP
			selection.providerName = fbName
			selection.modelName = e.getProviderModel(fbName, msg.Metadata)
			resp, err = selection.provider.Complete(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("调用失败（降级后）: %w", err)
			}
		}

		// G1: 记录 token 使用到 Budget (ProcessStream 路径)
		if useBudgetStream && resp.Usage.TotalTokens > 0 {
			budget.RecordTokens(resp.Usage.TotalTokens)
		}

		// 无 tool_calls → 最终轮，切换到流式返回
		if !resp.HasToolCalls() {
			// 重新发起流式请求 (不含上一次的 Complete 结果)
			llmStream, sErr := selection.provider.Stream(ctx, req)
			if sErr != nil {
				// 流式失败则直接用 Complete 结果
				return singleChunkWithTools(
					resp.Content,
					buildReplyMetadata(msg.Metadata, selection.providerName, selection.modelName, ""),
					allToolCalls,
				), nil
			}
			ch := make(chan *adapter.ReplyChunk, 16)
			go e.pipeStreamWithTools(ctx, ch, llmStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput, allToolCalls)
			return ch, nil
		}

		// G2: 结构化 tool transcript (ProcessStream 路径)
		var streamToolRefs []llm.ToolCallRef
		for _, tc := range resp.ToolCalls {
			streamToolRefs = append(streamToolRefs, llm.ToolCallRef{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		messages = append(messages, llm.AssistantToolCallMessage(resp.Content, streamToolRefs))

		for _, tc := range resp.ToolCalls {
			var toolArgs map[string]any
			if tc.Arguments != "" {
				json.Unmarshal([]byte(tc.Arguments), &toolArgs)
			}
			var toolResult string
			if e.toolExecutor != nil {
				toolResult, err = e.toolExecutor.Execute(ctx, tc.Name, toolArgs)
				if err != nil {
					log.Printf("[tool] %s 执行失败: %v", tc.Name, err)
					toolResult = fmt.Sprintf("Error: tool %q execution failed", tc.Name)
				}
			} else {
				toolResult = "Error: tool executor not available"
			}
			messages = append(messages, llm.ToolResultMessage(tc.ID, toolResult))
			argsJSON, _ := json.Marshal(toolArgs)
			allToolCalls = append(allToolCalls, adapter.ToolCall{
				ID: tc.ID, Name: tc.Name,
				Arguments: string(argsJSON),
				Result:    truncateResult(toolResult, 500),
			})
		}
	}

	// 超过最大轮次，用最后一次 Complete 结果
	log.Printf("流式工具循环达到上限 %d 轮", maxStreamToolTurns)
	lastResp, err := selection.provider.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("最终补全失败: %w", err)
	}
	return singleChunkWithTools(
		lastResp.Content,
		buildReplyMetadata(msg.Metadata, selection.providerName, selection.modelName, ""),
		allToolCalls,
	), nil
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
			ID:               "cost-" + idgen.ShortID(),
			UserID:           msg.UserID,
			Provider:         providerName,
			Model:            modelName,
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
			CreatedAt:        time.Now(),
		}
		if err := e.store.SaveCost(saveCtx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
		// Note: stream 最终轮的 token 记录在 ProcessStream 的 per-request budget 中
		// pipeStream 不再引用全局 budget (已改为 per-request)
	}

	// 上下文压缩（异步，G3: 串行化后台写入）
	if e.compactor != nil {
		e.bgWg.Add(1)
		go func() {
			defer e.bgWg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if p, ok := e.router.Get(providerName); ok && p != nil {
				if err := e.compactor.CompactIfNeeded(bgCtx, sessionID, p); err != nil {
					log.Printf("上下文压缩失败: %v", err)
				}
			}
		}()
	}
}

// pipeStreamWithTools 类似 pipeStream，但携带工具调用信息并在结束时释放 SessionLock
func (e *ReActEngine) pipeStreamWithTools(
	ctx context.Context,
	ch chan<- *adapter.ReplyChunk,
	llmStream *hexagon.LLMStream,
	sessionID string,
	msg *adapter.Message,
	providerName string,
	modelName string,
	cacheInput string,
	toolCalls []adapter.ToolCall,
) {
	// 释放 SessionLock (流式路径在 goroutine 结束时释放)
	if unlock, ok := ctx.Value("session_unlock").(func()); ok && unlock != nil {
		defer unlock()
	}

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

	result := llmStream.Result()
	content := fullContent.String()

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(saveCtx, sessionID, content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	} else {
		assistantMessageID = record.ID
	}

	e.cache.Put(cacheInput, content, providerName, modelName)

	doneChunk := &adapter.ReplyChunk{
		Done:      true,
		Metadata:  buildReplyMetadata(msg.Metadata, providerName, modelName, assistantMessageID),
		ToolCalls: toolCalls,
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

	if result != nil && result.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:               "cost-" + idgen.ShortID(),
			UserID:           msg.UserID,
			Provider:         providerName,
			Model:            modelName,
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
			CreatedAt:        time.Now(),
		}
		if err := e.store.SaveCost(saveCtx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
	}

	e.autoExtractMemory(msg.Content, content)
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
			ID:               "cost-" + idgen.ShortID(),
			UserID:           msg.UserID,
			Provider:         providerName,
			Model:            modelName,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CreatedAt:        time.Now(),
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

func translateProviderToolCalls(toolCalls []llm.ToolCall) []adapter.ToolCall {
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
