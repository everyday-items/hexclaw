package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

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
}

// NewReActEngine 创建 ReAct 引擎
func NewReActEngine(
	cfg *config.Config,
	router *llmrouter.Selector,
	store storage.Store,
	skills *skill.DefaultRegistry,
) *ReActEngine {
	// 初始化语义缓存
	cacheTTL := 24 * time.Hour
	if cfg.LLM.Cache.TTL != "" {
		if d, err := time.ParseDuration(cfg.LLM.Cache.TTL); err == nil {
			cacheTTL = d
		}
	}
	maxEntries := cfg.LLM.Cache.MaxEntries
	if maxEntries == 0 {
		maxEntries = 10000
	}
	llmCache := cache.New(cache.Options{
		Enabled:    cfg.LLM.Cache.Enabled,
		TTL:        cacheTTL,
		MaxEntries: maxEntries,
	})

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
	// 1. 获取或创建会话
	sess, err := e.sessions.GetOrCreate(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("会话管理失败: %w", err)
	}
	msg.SessionID = sess.ID

	// 2. 尝试快速路径: Skill 关键词匹配
	if matched, ok := e.skills.Match(msg); ok {
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
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

		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, result.Content); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}

		argsJSON, _ := json.Marshal(skillArgs)
		return &adapter.Reply{
			Content:  result.Content,
			Metadata: result.Metadata,
			ToolCalls: []adapter.ToolCall{{
				ID:        "tc-" + idgen.ShortID(),
				Name:      matched.Name(),
				Arguments: string(argsJSON),
				Result:    truncateResult(result.Content, 500),
			}},
		}, nil
	}

	// 3. 语义缓存查询
	if cached, ok := e.cache.Get(msg.Content, e.cfg.LLM.Default); ok {
		log.Printf("语义缓存命中: %s", msg.Content[:min(20, len(msg.Content))])
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, cached); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}
		return &adapter.Reply{
			Content:  cached,
			Metadata: map[string]string{"source": "cache"},
		}, nil
	}

	// 4. 主路径: 构建对话上下文（在 SaveUserMessage 之前，避免 history 重复包含当前消息）
	history, err := e.sessions.BuildContext(ctx, sess.ID)
	if err != nil {
		log.Printf("构建上下文失败: %v", err)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
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

	// 6. 获取 LLM Provider（支持指定 provider，"" 和 "auto" 使用默认路由）
	provider, providerName, err := e.resolveProvider(ctx, msg.Metadata["provider"], msg)
	if err != nil {
		return nil, fmt.Errorf("llm 路由失败: %w", err)
	}

	// 7. 创建 Agent（支持角色选择）
	roleName := msg.Metadata["role"] // 从消息元数据中获取角色
	reactAgent := e.createAgent(roleName, provider, msg.Metadata)

	// 构建 Agent 输入
	agentInput := hexagon.Input{
		Query: msg.Content,
		Context: map[string]any{
			"session_id": sess.ID,
			"user_id":    msg.UserID,
			"platform":   string(msg.Platform),
			"provider":   providerName,
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
		// 尝试降级到备用 Provider
		fallbackP, fbName, fbErr := e.router.Fallback(providerName)
		if fbErr != nil {
			return nil, fmt.Errorf("agent 执行失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 失败，降级到 %s: %v", providerName, fbName, err)

		reactAgent = e.createAgent(roleName, fallbackP, msg.Metadata)
		output, err = reactAgent.Run(ctx, agentInput)
		if err != nil {
			return nil, fmt.Errorf("agent 执行失败（降级后）: %w", err)
		}
		providerName = fbName
	}

	// 7. 保存助手回复
	if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, output.Content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	}

	// 8. 写入语义缓存（安全获取 model 名称，避免 map 访问空值）
	modelName := e.getProviderModel(providerName)
	e.cache.Put(msg.Content, output.Content, providerName, modelName)

	// 9. 记录 Token 使用（用于成本控制）
	if output.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:        "cost-" + idgen.ShortID(),
			UserID:    msg.UserID,
			Provider:  providerName,
			Model:     modelName,
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
			Provider:     providerName,
			Model:        modelName,
		}
	}

	replyMeta := map[string]string{
		"provider": providerName,
		"model":    modelName,
	}
	if msg.Metadata != nil {
		if v := msg.Metadata["route_source"]; v != "" {
			replyMeta["route_source"] = v
		}
		if v := msg.Metadata["routed_agent"]; v != "" {
			replyMeta["routed_agent"] = v
		}
	}

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
func (e *ReActEngine) getProviderModel(providerName string) string {
	if pc, ok := e.cfg.LLM.Providers[providerName]; ok {
		return pc.Model
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
	// 1. 获取或创建会话
	sess, err := e.sessions.GetOrCreate(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("会话管理失败: %w", err)
	}
	msg.SessionID = sess.ID

	// 2. 尝试快速路径: Skill 匹配 → 单 chunk 返回
	if matched, ok := e.skills.Match(msg); ok {
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
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
		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, result.Content); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}
		argsJSON, _ := json.Marshal(skillArgs)
		tc := []adapter.ToolCall{{
			ID:        "tc-" + idgen.ShortID(),
			Name:      matched.Name(),
			Arguments: string(argsJSON),
			Result:    truncateResult(result.Content, 500),
		}}
		return singleChunkWithTools(result.Content, tc), nil
	}

	// 3. 语义缓存命中 → 单 chunk 返回
	if cached, ok := e.cache.Get(msg.Content, e.cfg.LLM.Default); ok {
		log.Printf("语义缓存命中: %s", msg.Content[:min(20, len(msg.Content))])
		if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
			log.Printf("保存用户消息失败: %v", err)
		}
		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, cached); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}
		return singleChunk(cached), nil
	}

	// 4. 构建对话上下文（在保存用户消息之前，避免 history 中重复包含当前消息）
	history, err := e.sessions.BuildContext(ctx, sess.ID)
	if err != nil {
		log.Printf("构建上下文失败: %v", err)
	}

	// 5. 保存用户消息（在 BuildContext 之后，确保 history 不含当前消息）
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
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

	// 6. 获取 LLM Provider（支持指定 provider，"" 和 "auto" 使用默认路由）
	provider, providerName, err := e.resolveProvider(ctx, msg.Metadata["provider"], msg)
	if err != nil {
		return nil, fmt.Errorf("llm 路由失败: %w", err)
	}

	// 7. 构建 CompletionRequest（含 system prompt + 历史 + 知识库 + 用户消息）
	roleName := msg.Metadata["role"]
	messages := e.buildStreamMessages(roleName, history, kbContext, msg.Content, msg.Metadata)
	req := hexagon.CompletionRequest{
		Messages: messages,
	}
	if m := msg.Metadata["agent_model"]; m != "" {
		req.Model = m
	}
	if v := msg.Metadata["agent_max_tokens"]; v != "" {
		if n, err := fmt.Sscanf(v, "%d", new(int)); n == 1 && err == nil {
			fmt.Sscanf(v, "%d", &req.MaxTokens)
		}
	}
	if v := msg.Metadata["agent_temperature"]; v != "" {
		var t float64
		if _, err := fmt.Sscanf(v, "%f", &t); err == nil {
			req.Temperature = &t
		}
	}

	// 8. 调用 provider.Stream()
	llmStream, err := provider.Stream(ctx, req)
	if err != nil {
		// 降级到备用 Provider
		fallbackP, fbName, fbErr := e.router.Fallback(providerName)
		if fbErr != nil {
			return nil, fmt.Errorf("流式调用失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 流式调用失败，降级到 %s: %v", providerName, fbName, err)
		llmStream, err = fallbackP.Stream(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("流式调用失败（降级后）: %w", err)
		}
		providerName = fbName
	}

	// 9. 启动 goroutine 将 LLMStreamChunk 转发为 adapter.ReplyChunk
	ch := make(chan *adapter.ReplyChunk, 16)
	go e.pipeStream(ctx, ch, llmStream, sess.ID, msg, providerName)

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

	// 发送结束标记（携带 Usage 信息）
	doneChunk := &adapter.ReplyChunk{Done: true}
	if result != nil && result.Usage.TotalTokens > 0 {
		modelName := e.getProviderModel(providerName)
		doneChunk.Usage = &adapter.Usage{
			InputTokens:  result.Usage.PromptTokens,
			OutputTokens: result.Usage.CompletionTokens,
			TotalTokens:  result.Usage.TotalTokens,
			Provider:     providerName,
			Model:        modelName,
		}
	}
	ch <- doneChunk
	content := fullContent.String()

	// 使用独立 context 进行后续操作，避免请求 ctx 取消后无法保存
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	// 保存助手回复
	if err := e.sessions.SaveAssistantMessage(saveCtx, sessionID, content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	}

	// 写入语义缓存
	modelName := e.getProviderModel(providerName)
	e.cache.Put(msg.Content, content, providerName, modelName)

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
func (e *ReActEngine) buildStreamMessages(roleName string, history []hexagon.Message, kbContext, userQuery string, metadata map[string]string) []hexagon.Message {
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

	// 当前用户消息
	messages = append(messages, hexagon.Message{
		Role:    "user",
		Content: userQuery,
	})

	return messages
}

// singleChunk 将完整内容包装为单 chunk channel（用于快速路径）
func singleChunk(content string) <-chan *adapter.ReplyChunk {
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Content: content, Done: true}
	close(ch)
	return ch
}

func singleChunkWithTools(content string, toolCalls []adapter.ToolCall) <-chan *adapter.ReplyChunk {
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Content: content, Done: true, ToolCalls: toolCalls}
	close(ch)
	return ch
}

// resolveProvider 根据请求的 provider 名称解析 LLM Provider
//
// 如果 providerHint 为空或 "auto"，使用路由器默认策略选择；
// 否则尝试使用指定的 Provider，不存在则回退到默认。
func (e *ReActEngine) resolveProvider(ctx context.Context, providerHint string, msg *adapter.Message) (hexagon.Provider, string, error) {
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
		return e.router.Route(ctx)
	}
	if p, ok := e.router.Get(hint); ok {
		return p, hint, nil
	}
	log.Printf("指定的 Provider %q 不存在，回退到默认路由", hint)
	return e.router.Route(ctx)
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
