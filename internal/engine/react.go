package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/everyday-items/hexagon"
	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/agents"
	"github.com/everyday-items/hexclaw/internal/cache"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/knowledge"
	"github.com/everyday-items/hexclaw/internal/llmrouter"
	"github.com/everyday-items/hexclaw/internal/session"
	"github.com/everyday-items/hexclaw/internal/skill"
	"github.com/everyday-items/hexclaw/internal/storage"
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
	mu       sync.RWMutex
	cfg      *config.Config
	router   *llmrouter.Router
	sessions *session.Manager
	skills   *skill.DefaultRegistry
	store    storage.Store
	cache    *cache.Cache
	kb       *knowledge.Manager // 知识库管理器（可为 nil）
	factory  *agents.Factory    // Agent 角色工厂
	started  bool
	startAt  time.Time
}

// NewReActEngine 创建 ReAct 引擎
func NewReActEngine(
	cfg *config.Config,
	router *llmrouter.Router,
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
	llmCache := cache.New(cache.Config{
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
	e.kb = kb
}

// KnowledgeBase 获取知识库管理器
func (e *ReActEngine) KnowledgeBase() *knowledge.Manager {
	return e.kb
}

// AgentFactory 获取 Agent 角色工厂
func (e *ReActEngine) AgentFactory() *agents.Factory {
	return e.factory
}

// Start 启动引擎
func (e *ReActEngine) Start(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = true
	e.startAt = time.Now()
	log.Println("Agent 引擎已启动")
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

	// 2. 保存用户消息
	if err := e.sessions.SaveUserMessage(ctx, sess.ID, msg.Content); err != nil {
		log.Printf("保存用户消息失败: %v", err)
	}

	// 3. 尝试快速路径: Skill 关键词匹配
	if matched, ok := e.skills.Match(msg); ok {
		result, err := matched.Execute(ctx, map[string]any{
			"query":   msg.Content,
			"user_id": msg.UserID,
		})
		if err != nil {
			return nil, fmt.Errorf("Skill %s 执行失败: %w", matched.Name(), err)
		}

		// 保存助手回复
		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, result.Content); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}

		return &adapter.Reply{
			Content:  result.Content,
			Metadata: result.Metadata,
		}, nil
	}

	// 4. 语义缓存查询
	if cached, ok := e.cache.Get(msg.Content); ok {
		log.Printf("语义缓存命中: %s", msg.Content[:min(20, len(msg.Content))])
		if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, cached); err != nil {
			log.Printf("保存助手回复失败: %v", err)
		}
		return &adapter.Reply{
			Content:  cached,
			Metadata: map[string]string{"source": "cache"},
		}, nil
	}

	// 5. 主路径: 构建对话上下文
	history, err := e.sessions.BuildContext(ctx, sess.ID)
	if err != nil {
		log.Printf("构建上下文失败: %v", err)
		// 不中断，继续无上下文处理
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

	// 6. 获取 LLM Provider
	provider, providerName, err := e.router.Route(ctx)
	if err != nil {
		return nil, fmt.Errorf("LLM 路由失败: %w", err)
	}

	// 7. 创建 Agent（支持角色选择）
	roleName := msg.Metadata["role"] // 从消息元数据中获取角色
	var reactAgent hexagon.Agent

	if roleName != "" {
		// 使用指定角色创建 Agent
		agent, roleErr := e.factory.CreateAgent(roleName, provider)
		if roleErr != nil {
			log.Printf("创建角色 Agent 失败: %v，降级到默认", roleErr)
			reactAgent = hexagon.NewReActAgent(
				hexagon.AgentWithName("hexclaw"),
				hexagon.AgentWithLLM(provider),
				hexagon.AgentWithSystemPrompt(systemPrompt),
				hexagon.AgentWithMaxIterations(10),
			)
		} else {
			reactAgent = agent
		}
	} else {
		// 默认 Agent
		reactAgent = hexagon.NewReActAgent(
			hexagon.AgentWithName("hexclaw"),
			hexagon.AgentWithLLM(provider),
			hexagon.AgentWithSystemPrompt(systemPrompt),
			hexagon.AgentWithMaxIterations(10),
		)
	}

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
			return nil, fmt.Errorf("Agent 执行失败且无可用备用: %w", err)
		}
		log.Printf("Provider %s 失败，降级到 %s: %v", providerName, fbName, err)

		// 使用备用 Provider 重建 Agent
		if roleName != "" {
			reactAgent, _ = e.factory.CreateAgent(roleName, fallbackP)
		}
		if reactAgent == nil {
			reactAgent = hexagon.NewReActAgent(
				hexagon.AgentWithName("hexclaw"),
				hexagon.AgentWithLLM(fallbackP),
				hexagon.AgentWithSystemPrompt(systemPrompt),
				hexagon.AgentWithMaxIterations(10),
			)
		}
		output, err = reactAgent.Run(ctx, agentInput)
		if err != nil {
			return nil, fmt.Errorf("Agent 执行失败（降级后）: %w", err)
		}
		providerName = fbName
	}

	// 7. 保存助手回复
	if err := e.sessions.SaveAssistantMessage(ctx, sess.ID, output.Content); err != nil {
		log.Printf("保存助手回复失败: %v", err)
	}

	// 8. 写入语义缓存
	e.cache.Put(msg.Content, output.Content, providerName, e.cfg.LLM.Providers[providerName].Model)

	// 9. 记录 Token 使用（用于成本控制）
	if output.Usage.TotalTokens > 0 {
		costRecord := &storage.CostRecord{
			ID:        "cost-" + fmt.Sprintf("%d", time.Now().UnixNano()),
			UserID:    msg.UserID,
			Provider:  providerName,
			Model:     e.cfg.LLM.Providers[providerName].Model,
			Tokens:    output.Usage.TotalTokens,
			CreatedAt: time.Now(),
		}
		if err := e.store.SaveCost(ctx, costRecord); err != nil {
			log.Printf("记录成本失败: %v", err)
		}
	}

	return &adapter.Reply{
		Content: output.Content,
		Metadata: map[string]string{
			"provider": providerName,
			"model":    e.cfg.LLM.Providers[providerName].Model,
		},
	}, nil
}

// ProcessStream 流式处理消息
//
// 当前实现：先同步处理再包装为流式输出。
// TODO: 后续接入 Hexagon Agent 的原生流式能力。
func (e *ReActEngine) ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	ch := make(chan *adapter.ReplyChunk, 1)

	go func() {
		defer close(ch)

		reply, err := e.Process(ctx, msg)
		if err != nil {
			ch <- &adapter.ReplyChunk{Error: err, Done: true}
			return
		}

		// 将完整回复作为单个 chunk 输出
		ch <- &adapter.ReplyChunk{
			Content: reply.Content,
			Done:    true,
		}
	}()

	return ch, nil
}

// systemPrompt HexClaw 系统提示词
const systemPrompt = `你是 HexClaw，一个安全、智能、高效的个人 AI 助手。

你的核心原则：
1. 安全第一：不执行危险操作，不泄露敏感信息
2. 诚实可靠：不确定的事情坦诚告知，不编造信息
3. 简洁高效：直接回答问题，避免冗长的废话
4. 友好专业：用友好的语气提供专业的帮助

你可以帮助用户：
- 回答问题和提供建议
- 搜索信息和整理资料
- 翻译文本
- 编写和解释代码
- 分析和总结文本

请用中文回答，除非用户明确要求使用其他语言。`
