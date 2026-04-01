package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
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
	"github.com/hexagon-codes/hexclaw/trace"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

type ctxKey string

const ctxKeySessionUnlock ctxKey = "session_unlock"

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
func (e *ReActEngine) Stop(ctx context.Context) error {
	// G3: 等待后台 goroutine（压缩/记忆提取）完成，防止 DB close 后写入
	e.bgWg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = false
	trace.L(ctx).Info("Agent 引擎已停止")
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
	if trace.L(ctx) == slog.Default() {
		ctx = trace.WithLogger(ctx, trace.NewRequest(msg.UserID, msg.SessionID))
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
			trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
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
			trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sess.ID)
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
		trace.L(ctx).Info("语义缓存命中", "query", msg.Content[:min(20, len(msg.Content))], "session", sess.ID)
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
		}
		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, cached); err != nil {
			trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sess.ID)
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
		trace.L(ctx).Error("构建上下文失败", "err", err, "session", sess.ID)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
		trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
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
			trace.L(ctx).Error("知识库检索失败", "err", kbErr, "session", sess.ID)
		} else if kbResult != "" {
			kbContext = kbResult
			trace.L(ctx).Info("知识库命中", "query", msg.Content[:min(20, len(msg.Content))], "session", sess.ID)
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

	// 收集工具定义（本地模型跳过）
	var tools []llm.ToolDefinition
	isLocal := strings.Contains(providerName, "本地") ||
		strings.Contains(strings.ToLower(providerName), "ollama") ||
		strings.Contains(strings.ToLower(providerName), "local")
	if e.toolCollector != nil && !isLocal {
		tools = e.toolCollector.Collect()
	}

	// 构建初始请求
	req := e.buildCompletionRequest(msg, history, kbContext)
	if len(tools) > 0 {
		req.Tools = tools
	}
	// 本地 thinking 模型注入 /no_think（与流式路径对齐）
	if isLocal && msg.Metadata["thinking"] != "on" && isLocalThinkingModel(modelName) {
		injectNoThink(req.Messages)
		trace.L(ctx).Info("注入 /no_think", "model", modelName)
	}

	// 无工具时直接 Complete，不走工具循环
	if len(tools) == 0 {
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			if !explicitProvider {
				fallbackP, fbName, fbErr := e.router.Fallback(providerName)
				if fbErr == nil {
					trace.L(ctx).Warn("Provider 降级", "from", providerName, "to", fbName, "err", err, "session", sessionID)
					provider = fallbackP
					providerName = fbName
					modelName = e.getProviderModel(fbName, msg.Metadata)
					resp, err = provider.Complete(ctx, req)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("provider %s 调用失败: %w", providerName, err)
			}
		}
		return e.finalizeReply(ctx, sessionID, msg, resp, providerName, modelName, cacheInput, nil)
	}

	var allToolCalls []adapter.ToolCall
	messages := req.Messages

	for turn := 0; turn < hardMaxTurns; turn++ {
		// Budget 检查 (每轮开始前)
		if useBudget {
			if err := budget.Check(); err != nil {
				trace.L(ctx).Warn("预算耗尽", "turn", turn, "err", err, "session", sessionID)
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
			trace.L(ctx).Warn("Provider 降级", "from", providerName, "to", fbName, "err", err, "session", sessionID)
			provider = fallbackP
			providerName = fbName
			modelName = e.getProviderModel(fbName, msg.Metadata)
			// 降级后需重新收集工具（不同 provider 可能支持不同数量）
			resp, err = provider.Complete(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("补全失败（降级后）: %w", err)
			}
		}

		// 当 provider 未返回 token 统计时，使用 tokenizer 估算
		if resp.Usage.TotalTokens == 0 {
			p, c, t := estimateResponseUsage(providerName, messages, resp.Content)
			resp.Usage.PromptTokens = p
			resp.Usage.CompletionTokens = c
			resp.Usage.TotalTokens = t
		}

		// 记录 token 使用到 Budget
		if useBudget && resp.Usage.TotalTokens > 0 {
			budget.RecordTokens(resp.Usage.TotalTokens)
			if cost := EstimateCost(providerName, modelName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens); cost > 0 {
				budget.RecordCost(cost)
			}
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
				if uerr := json.Unmarshal([]byte(tc.Arguments), &toolArgs); uerr != nil {
					trace.L(ctx).Error("工具参数解析失败", "tool", tc.Name, "err", uerr, "session", sessionID)
				}
			}

			var toolResult string
			if e.toolExecutor != nil {
				toolResult, err = e.toolExecutor.Execute(ctx, tc.Name, toolArgs)
				if err != nil {
					trace.L(ctx).Error("工具执行失败", "tool", tc.Name, "err", err, "session", sessionID)
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
		trace.L(ctx).Warn("预算耗尽，工具循环结束", "summary", budget.Summary(), "session", sessionID)
	} else {
		trace.L(ctx).Warn("工具循环达到硬限", "maxTurns", 5, "session", sessionID)
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
		trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sessionID)
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
			trace.L(ctx).Error("记录成本失败", "err", err, "session", sessionID)
		}
		// Note: token 已在 completeWithTools 循环中按轮记录到 per-request budget
	}

	// 自动记忆提取（异步）
	e.autoExtractMemory(msg.Content, resp.Content)

	// 上下文压缩（异步，G3: 串行化后台写入）
	if e.compactor != nil {
		reqLogger := trace.L(ctx) // 捕获请求 logger 传入 goroutine
		e.bgWg.Add(1)
		go func() {
			defer e.bgWg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			bgCtx = trace.WithLogger(bgCtx, reqLogger)
			if err := e.compactor.CompactIfNeeded(bgCtx, sessionID, nil); err != nil {
				trace.L(bgCtx).Error("上下文压缩失败", "err", err, "session", sessionID)
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
			trace.L(context.Background()).Error("创建角色 Agent 失败", "err", err, "role", roleName)
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
	if trace.L(ctx) == slog.Default() {
		ctx = trace.WithLogger(ctx, trace.NewRequest(msg.UserID, msg.SessionID))
	}

	// 1. 获取或创建会话
	sess, err := e.sessions.GetOrCreate(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("会话管理失败: %w", err)
	}
	msg.SessionID = sess.ID

	// 1.5 Session 锁
	// unlock 存入 ctx，由 pipeStream/pipeStreamWithTools goroutine 在结束时释放。
	// 对于提前 return 的路径（Skill/缓存/Stream 失败），必须在此 defer 兜底释放。
	var sessionUnlock func()
	if e.sessionLock != nil {
		sessionUnlock = e.sessionLock.Acquire(sess.ID)
		ctx = context.WithValue(ctx, ctxKeySessionUnlock, sessionUnlock)
	}
	// unlockOnReturn 标记：goroutine 启动后设为 false，防止 defer 重复释放
	goroutineLaunched := false
	defer func() {
		if !goroutineLaunched && sessionUnlock != nil {
			sessionUnlock()
		}
	}()

	// 2. 尝试快速路径: Skill 匹配 → 单 chunk 返回
	if matched, ok := e.skills.Match(msg); ok {
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
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
			trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sess.ID)
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
		trace.L(ctx).Info("语义缓存命中", "query", msg.Content[:min(20, len(msg.Content))], "session", sess.ID)
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
			trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
		}
		assistantMessageID := ""
		if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sess.ID, cached); err != nil {
			trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sess.ID)
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
		trace.L(ctx).Error("构建上下文失败", "err", err, "session", sess.ID)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg); err != nil {
		trace.L(ctx).Error("保存用户消息失败", "err", err, "session", sess.ID)
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
			trace.L(ctx).Error("知识库检索失败", "err", kbErr, "session", sess.ID)
		} else if kbResult != "" {
			kbContext = kbResult
			trace.L(ctx).Info("知识库命中", "query", msg.Content[:min(20, len(msg.Content))], "session", sess.ID)
		}
	}

	// 7. 构建 CompletionRequest（含 tools + system prompt + 历史 + 知识库 + 用户消息）
	req := e.buildCompletionRequest(msg, history, kbContext)
	var tools []llm.ToolDefinition
	if e.toolCollector != nil {
		tools = e.toolCollector.Collect()
	}
	// 本地模型（Ollama）不注入工具定义 — 小模型处理大量工具描述极慢
	isLocal := strings.Contains(selection.providerName, "本地") ||
		strings.Contains(strings.ToLower(selection.providerName), "ollama") ||
		strings.Contains(strings.ToLower(selection.providerName), "local")
	if isLocal {
		tools = nil
		// 本地 thinking 模型（qwen3、deepseek-r1 等）：前端未显式开启 thinking 时注入 /no_think
		if msg.Metadata["thinking"] != "on" && isLocalThinkingModel(selection.modelName) {
			injectNoThink(req.Messages)
			trace.L(ctx).Info("注入 /no_think", "model", selection.modelName)
		}
	}
	trace.L(ctx).Info("LLM 调用准备", "tools", len(tools), "provider", selection.providerName, "model", selection.modelName, "local", isLocal)
	if len(tools) > 0 {
		req.Tools = tools
	}

	// 7.5 第一轮直接流式推给前端（thinking + 回复实时可见）

	llmStream, err := selection.provider.Stream(ctx, req)
	if err != nil {
		if !selection.explicitProvider {
			fallbackP, fbName, fbErr := e.router.Fallback(selection.providerName)
			if fbErr == nil {
				trace.L(ctx).Warn("Provider 降级", "from", selection.providerName, "to", fbName, "err", err)
				selection.provider = fallbackP
				selection.providerName = fbName
				selection.modelName = e.getProviderModel(fbName, msg.Metadata)
				llmStream, err = selection.provider.Stream(ctx, req)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("provider %s 调用失败: %w", selection.providerName, err)
		}
	}

	// 第一轮直接推流（pipeStream 内部完成保存/缓存等后处理）
	goroutineLaunched = true // goroutine 内的 defer unlock() 接管锁释放
	if len(tools) == 0 {
		ch := make(chan *adapter.ReplyChunk, 16)
		go e.pipeStream(ctx, ch, llmStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput)
		return ch, nil
	}

	// 有工具：直接推流给前端（thinking + 回复实时可见）
	ch := make(chan *adapter.ReplyChunk, 16)
	go e.pipeStreamWithTools(ctx, ch, llmStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput, nil)
	return ch, nil
}

// processStreamToolLoop 多轮工具循环（后续版本启用）
func (e *ReActEngine) processStreamToolLoop(
	ctx context.Context,
	req hexagon.CompletionRequest,
	selection *llmSelection,
	sess *storage.Session,
	msg *adapter.Message,
	cacheInput string,
) (<-chan *adapter.ReplyChunk, error) {
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
				trace.L(ctx).Warn("流式预算耗尽", "turn", turn, "err", err, "session", sess.ID)
				break
			}
		}
		req.Messages = messages

		llmStream, err := selection.provider.Stream(ctx, req)
		if err != nil {
			if selection.explicitProvider || turn > 0 {
				return nil, fmt.Errorf("provider %s 调用失败: %w", selection.providerName, err)
			}
			fallbackP, fbName, fbErr := e.router.Fallback(selection.providerName)
			if fbErr != nil {
				return nil, fmt.Errorf("调用失败且无可用备用: %w", err)
			}
			trace.L(ctx).Warn("Provider 降级", "from", selection.providerName, "to", fbName, "err", err, "session", sess.ID)
			selection.provider = fallbackP
			selection.providerName = fbName
			selection.modelName = e.getProviderModel(fbName, msg.Metadata)
			llmStream, err = selection.provider.Stream(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("调用失败（降级后）: %w", err)
			}
		}

		// 消费流，收集完整结果（用于检测 tool_calls）
		for range llmStream.Chunks() {
			// drain chunks — 中间轮不推给前端
		}
		result := llmStream.Result()

		// 当 provider 未返回 token 统计时，使用 tokenizer 估算
		if result.Usage.TotalTokens == 0 {
			p, c, t := estimateResponseUsage(selection.providerName, messages, result.Content)
			result.Usage.PromptTokens = p
			result.Usage.CompletionTokens = c
			result.Usage.TotalTokens = t
		}

		// G1: 记录 token 使用到 Budget (ProcessStream 路径)
		if useBudgetStream && result.Usage.TotalTokens > 0 {
			budget.RecordTokens(result.Usage.TotalTokens)
			if cost := EstimateCost(selection.providerName, selection.modelName, result.Usage.PromptTokens, result.Usage.CompletionTokens); cost > 0 {
				budget.RecordCost(cost)
			}
		}

		// 无 tool_calls → 最终轮，重新发起流式请求推给前端
		hasToolCalls := len(result.ToolCalls) > 0
		if !hasToolCalls {
			finalStream, sErr := selection.provider.Stream(ctx, req)
			if sErr != nil {
				// 流式失败则直接用已收集的结果
				return singleChunkWithTools(
					result.Content,
					buildReplyMetadata(msg.Metadata, selection.providerName, selection.modelName, ""),
					allToolCalls,
				), nil
			}
			ch := make(chan *adapter.ReplyChunk, 16)
			go e.pipeStreamWithTools(ctx, ch, finalStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput, allToolCalls)
			return ch, nil
		}

		// 有 tool_calls → 构建 tool transcript 并执行
		// G2: 结构化 tool transcript (ProcessStream 路径)
		var streamToolRefs []llm.ToolCallRef
		for _, tc := range result.ToolCalls {
			streamToolRefs = append(streamToolRefs, llm.ToolCallRef{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		messages = append(messages, llm.AssistantToolCallMessage(result.Content, streamToolRefs))

		for _, tc := range result.ToolCalls {
			var toolArgs map[string]any
			if tc.Arguments != "" {
				if uerr := json.Unmarshal([]byte(tc.Arguments), &toolArgs); uerr != nil {
					trace.L(ctx).Error("工具参数解析失败", "tool", tc.Name, "err", uerr, "session", sess.ID)
				}
			}
			var toolResult string
			if e.toolExecutor != nil {
				toolResult, err = e.toolExecutor.Execute(ctx, tc.Name, toolArgs)
				if err != nil {
					trace.L(ctx).Error("工具执行失败", "tool", tc.Name, "err", err, "session", sess.ID)
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

	// 超过最大轮次，用最后一次流式结果
	trace.L(ctx).Warn("流式工具循环达到上限", "maxTurns", maxStreamToolTurns, "session", sess.ID)
	finalStream, err := selection.provider.Stream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("最终补全失败: %w", err)
	}
	ch := make(chan *adapter.ReplyChunk, 16)
	go e.pipeStreamWithTools(ctx, ch, finalStream, sess.ID, msg, selection.providerName, selection.modelName, cacheInput, allToolCalls)
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
	// 释放 SessionLock — 与 pipeStreamWithTools 对齐，避免无工具路径死锁
	if unlock, ok := ctx.Value(ctxKeySessionUnlock).(func()); ok && unlock != nil {
		defer unlock()
	}
	defer close(ch)
	defer llmStream.Close()

	var fullContent strings.Builder

	reasoningLogged := false
	for chunk := range llmStream.Chunks() {
		if chunk.Content == "" && chunk.Reasoning == "" {
			continue
		}
		if chunk.Reasoning != "" && !reasoningLogged {
			trace.L(ctx).Info("首个 chunk", "type", "reasoning", "preview", chunk.Reasoning[:min(50, len(chunk.Reasoning))])
			reasoningLogged = true
		}
		fullContent.WriteString(chunk.Content)

		select {
		case ch <- &adapter.ReplyChunk{Content: chunk.Content, Reasoning: chunk.Reasoning}:
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
	saveCtx = trace.WithLogger(saveCtx, trace.L(ctx))

	// 保存助手回复
	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(saveCtx, sessionID, content); err != nil {
		trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sessionID)
	} else {
		assistantMessageID = record.ID
	}

	// 写入语义缓存
	e.cache.Put(cacheInput, content, providerName, modelName)

	// 当 provider 未返回 token 统计时，使用 tokenizer 估算 (streaming 路径仅估算 completion)
	if result != nil && result.Usage.TotalTokens == 0 && content != "" {
		_, c, t := estimateResponseUsage(providerName, nil, content)
		result.Usage.CompletionTokens = c
		result.Usage.TotalTokens = t
	}

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
			trace.L(ctx).Error("记录成本失败", "err", err, "session", sessionID)
		}
		// Note: stream 最终轮的 token 记录在 ProcessStream 的 per-request budget 中
		// pipeStream 不再引用全局 budget (已改为 per-request)
	}

	// 上下文压缩（异步，G3: 串行化后台写入）
	if e.compactor != nil {
		pipeLogger := trace.L(ctx)
		e.bgWg.Add(1)
		go func() {
			defer e.bgWg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			bgCtx = trace.WithLogger(bgCtx, pipeLogger)
			if p, ok := e.router.Get(providerName); ok && p != nil {
				if err := e.compactor.CompactIfNeeded(bgCtx, sessionID, p); err != nil {
					trace.L(bgCtx).Error("上下文压缩失败", "err", err, "session", sessionID)
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
	if unlock, ok := ctx.Value(ctxKeySessionUnlock).(func()); ok && unlock != nil {
		defer unlock()
	}

	defer close(ch)
	defer llmStream.Close()

	var fullContent strings.Builder
	for chunk := range llmStream.Chunks() {
		if chunk.Content == "" && chunk.Reasoning == "" {
			continue
		}
		fullContent.WriteString(chunk.Content)
		select {
		case ch <- &adapter.ReplyChunk{Content: chunk.Content, Reasoning: chunk.Reasoning}:
		case <-ctx.Done():
			ch <- &adapter.ReplyChunk{Error: ctx.Err(), Done: true}
			return
		}
	}

	result := llmStream.Result()
	content := fullContent.String()

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()
	saveCtx = trace.WithLogger(saveCtx, trace.L(ctx))

	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(saveCtx, sessionID, content); err != nil {
		trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sessionID)
	} else {
		assistantMessageID = record.ID
	}

	e.cache.Put(cacheInput, content, providerName, modelName)

	// 当 provider 未返回 token 统计时，使用 tokenizer 估算 (streaming 路径仅估算 completion)
	if result != nil && result.Usage.TotalTokens == 0 && content != "" {
		_, c, t := estimateResponseUsage(providerName, nil, content)
		result.Usage.CompletionTokens = c
		result.Usage.TotalTokens = t
	}

	// 合并参数传入的 toolCalls 和 result 中 LLM 返回的 ToolCalls
	finalToolCalls := toolCalls
	if result != nil && len(result.ToolCalls) > 0 {
		for _, tc := range result.ToolCalls {
			finalToolCalls = append(finalToolCalls, adapter.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
		trace.L(ctx).Info("LLM 返回工具调用", "count", len(result.ToolCalls),
			"tools", func() string {
				names := make([]string, len(result.ToolCalls))
				for i, t := range result.ToolCalls {
					names[i] = t.Name
				}
				return strings.Join(names, ",")
			}(), "session", sessionID)
	}
	doneChunk := &adapter.ReplyChunk{
		Done:      true,
		Metadata:  buildReplyMetadata(msg.Metadata, providerName, modelName, assistantMessageID),
		ToolCalls: finalToolCalls,
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
			trace.L(ctx).Error("记录成本失败", "err", err, "session", sessionID)
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
	// 追加能力上下文：知识库文件列表、Skill/MCP 工具、记忆
	sysContent += e.buildCapabilityContext(context.Background())
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
		trace.L(ctx).Warn("Provider 多模态降级", "from", providerName, "to", fbName, "err", err, "session", sessionID)
		resp, err = fallbackP.Complete(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("多模态补全失败（降级后）: %w", err)
		}
		providerName = fbName
		modelName = e.getProviderModel(fbName, msg.Metadata)
	}

	// 当 provider 未返回 token 统计时，使用 tokenizer 估算
	if resp.Usage.TotalTokens == 0 {
		p, c, t := estimateResponseUsage(providerName, req.Messages, resp.Content)
		resp.Usage.PromptTokens = p
		resp.Usage.CompletionTokens = c
		resp.Usage.TotalTokens = t
	}

	assistantMessageID := ""
	if record, err := e.sessions.SaveAssistantMessageRecord(ctx, sessionID, resp.Content); err != nil {
		trace.L(ctx).Error("保存助手回复失败", "err", err, "session", sessionID)
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
			trace.L(ctx).Error("记录成本失败", "err", err, "session", sessionID)
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

// buildCapabilityContext 构建能力上下文，注入 system prompt 末尾
//
// 让模型了解自己当前的能力：知识库文档列表、Skill/MCP 工具、长期记忆。
// 参考 Claude Projects / Coze 的设计：模型应知道自己能做什么。
func (e *ReActEngine) buildCapabilityContext(ctx context.Context) string {
	var sb strings.Builder

	// 1. 知识库文档列表
	if e.kb != nil && e.cfg.Knowledge.Enabled {
		if docs, err := e.kb.ListDocuments(ctx); err == nil && len(docs) > 0 {
			sb.WriteString("\n\n[你的知识库]\n")
			sb.WriteString("用户已上传以下文档，你可以基于这些文档回答问题：\n")
			for _, d := range docs {
				title := d.Title
				if title == "" {
					title = d.Source
				}
				sb.WriteString("- " + title + "\n")
			}
		}
	}

	// 2. Skill/MCP 工具列表
	if e.toolCollector != nil {
		tools := e.toolCollector.Collect()
		if len(tools) > 0 {
			sb.WriteString("\n[你可用的工具]\n")
			sb.WriteString("你可以调用以下工具来完成任务：\n")
			for _, t := range tools {
				desc := t.Function.Description
				if len(desc) > 60 {
					desc = desc[:60] + "..."
				}
				sb.WriteString("- " + t.Function.Name + "：" + desc + "\n")
			}
		}
	}

	// 3. 长期记忆
	if e.fileMem != nil {
		if mem := e.fileMem.GetMemory(); mem != "" {
			if len(mem) > 500 {
				mem = mem[:500] + "\n..."
			}
			sb.WriteString("\n[用户长期记忆]\n")
			sb.WriteString("以下是你对该用户的了解（跨会话持久记忆）：\n")
			sb.WriteString(mem + "\n")
		}
	}

	// 4. 环境感知
	sb.WriteString("\n[当前环境]\n")
	sb.WriteString("- 当前时间：" + time.Now().Format("2006-01-02 15:04 (Monday)") + "\n")
	sb.WriteString("- 操作系统：" + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	e.mu.RLock()
	router := e.router
	agentRtr := e.agentRouter
	cfg := e.cfg
	e.mu.RUnlock()

	// 当前模型
	if router != nil {
		if p, name, err := router.Route(ctx); err == nil && p != nil {
			model := router.ProviderModel(name)
			sb.WriteString("- 当前模型：" + name + " / " + model + "\n")
		}
		// 可用 Provider 列表
		providers := router.Providers()
		if len(providers) > 0 {
			sb.WriteString("- 可用模型：" + strings.Join(providers, "、") + "\n")
		}
	}

	// 5. Agent 列表
	if agentRtr != nil {
		if agents := agentRtr.ListAgents(); len(agents) > 0 {
			sb.WriteString("\n[已配置的 Agent]\n")
			for _, a := range agents {
				desc := a.Description
				if desc == "" {
					desc = a.DisplayName
				}
				if len(desc) > 50 {
					desc = desc[:50] + "..."
				}
				sb.WriteString("- @" + a.Name + "：" + desc + "\n")
			}
		}
	}

	// 6. 应用设置摘要
	if cfg != nil {
		sb.WriteString("\n[应用设置]\n")
		sb.WriteString("- 知识库：" + boolZh(cfg.Knowledge.Enabled) + "\n")
		sb.WriteString("- MCP 工具：" + boolZh(cfg.MCP.Enabled) + "\n")
		sb.WriteString("- 定时任务：" + boolZh(cfg.Cron.Enabled) + "\n")
		sb.WriteString("- Webhook：" + boolZh(cfg.Webhook.Enabled) + "\n")
		sb.WriteString("- 长期记忆：" + boolZh(cfg.FileMemory.Enabled) + "\n")
	}

	return sb.String()
}

func boolZh(b bool) string {
	if b {
		return "已启用"
	}
	return "未启用"
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

// isLocalThinkingModel 检测是否为支持 thinking 模式的本地模型
func isLocalThinkingModel(model string) bool {
	m := strings.ToLower(model)
	// qwen3 系列、deepseek-r1 系列默认开启 thinking
	return strings.Contains(m, "qwen3") ||
		strings.Contains(m, "deepseek-r1") ||
		strings.Contains(m, "qwq")
}

// injectNoThink 在请求的最后一条用户消息中追加 /no_think 指令，兼容纯文本和多模态消息
func injectNoThink(messages []hexagon.Message) {
	last := len(messages) - 1
	if last < 0 || messages[last].Role != "user" {
		return
	}
	if messages[last].HasMultiContent() {
		// 多模态消息：找到第一个 text part 追加
		for i, part := range messages[last].MultiContent {
			if part.Type == "text" {
				messages[last].MultiContent[i].Text += " /no_think"
				return
			}
		}
	} else {
		messages[last].Content += " /no_think"
	}
}

func truncateResult(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
