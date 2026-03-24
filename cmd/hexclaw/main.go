// Package main 是 HexClaw CLI 的入口
//
// HexClaw 是企业级安全的个人 AI Agent，支持多平台接入、六层安全网关、
// LLM 智能路由、Skill 沙箱执行等能力。
//
// 用法:
//
//	hexclaw serve              # 启动服务
//	hexclaw init               # 初始化配置
//	hexclaw skill list         # 列出已安装的 Skill
//	hexclaw version            # 版本信息
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hexagon-codes/hexagon"
	aiopenai "github.com/hexagon-codes/ai-core/llm/openai"
	ragembedder "github.com/hexagon-codes/hexagon/rag/embedder"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/adapter"
	webadapter "github.com/hexagon-codes/hexclaw/adapter/web"
	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/audit"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/desktop"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/gateway"
	"github.com/hexagon-codes/hexclaw/heartbeat"
	"github.com/hexagon-codes/hexclaw/instances"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/hexclaw/voice"
	"github.com/hexagon-codes/hexclaw/webhook"
)

// 版本信息，通过 -ldflags 注入
var (
	version = "v0.0.2"
	commit  = "none"
	date    = "unknown"
)

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd 创建根命令
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "hexclaw",
		Short: "HexClaw - 企业级安全的个人 AI Agent",
		Long: `HexClaw 是企业级安全的个人 AI Agent。
安全 + 开源 + 自托管 + 易用 + 功能全面。

快速开始:
  export DEEPSEEK_API_KEY="sk-xxx"
  hexclaw serve`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newServeCmd(),
		newInitCmd(),
		newVersionCmd(),
		newSkillCmd(),
		newSecurityCmd(),
	)

	return root
}

// newServeCmd 创建 serve 子命令
func newServeCmd() *cobra.Command {
	var (
		configFile    string
		feishuAppID   string
		feishuSecret  string
		telegramToken string
		desktopMode   bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 HexClaw 服务",
		Long: `启动 HexClaw 服务，包含 Agent 引擎、安全网关、平台适配器、REST API。

示例:
  hexclaw serve
  hexclaw serve --config hexclaw.yaml
  hexclaw serve --desktop
  hexclaw serve --feishu-app-id xxx --feishu-app-secret xxx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(configFile, feishuAppID, feishuSecret, telegramToken, desktopMode)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "", "配置文件路径 (默认 ~/.hexclaw/hexclaw.yaml)")
	cmd.Flags().StringVar(&feishuAppID, "feishu-app-id", "", "飞书 App ID")
	cmd.Flags().StringVar(&feishuSecret, "feishu-app-secret", "", "飞书 App Secret")
	cmd.Flags().StringVar(&telegramToken, "telegram-token", "", "Telegram Bot Token")
	cmd.Flags().BoolVar(&desktopMode, "desktop", false, "桌面客户端模式（仅监听 localhost）")

	return cmd
}

// newInitCmd 创建 init 子命令
func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "初始化配置文件",
		Long:  "在 ~/.hexclaw/ 目录下生成默认配置文件 hexclaw.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit()
		},
	}
}

// newVersionCmd 创建 version 子命令
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("HexClaw %s\n", version)
			fmt.Printf("  Commit: %s\n", commit)
			fmt.Printf("  Built:  %s\n", date)
		},
	}
}

// newSkillCmd 创建 skill 子命令组
func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Skill 管理",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "列出已安装的 Skill",
			RunE: func(cmd *cobra.Command, args []string) error {
				fmt.Println("已安装的 Skill:")
				fmt.Println("  search    - 网络搜索 (内置)")
				fmt.Println("  weather   - 天气查询 (内置)")
				fmt.Println("  translate - 翻译     (内置)")
				fmt.Println("  summary   - 文本摘要 (内置)")
				return nil
			},
		},
	)

	return cmd
}

// runServe 启动服务主流程
//
// 初始化顺序：配置 → 存储 → LLM 路由 → Skill → 引擎 → HTTP 服务
func runServe(configFile, feishuAppID, feishuSecret, telegramToken string, desktopMode bool) error {
	// 1. 加载配置
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 桌面客户端模式：仅监听 localhost，自动启用 WebSocket，跳过认证
	if desktopMode {
		cfg.Server.Host = "127.0.0.1"
		cfg.Platforms.Web.Enabled = true
		cfg.Security.Auth.AllowAnonymous = true
		cfg.Cron.Enabled = true
		cfg.Canvas.Enabled = true
	}

	// 命令行参数覆盖配置文件（向 slice 首元素写入，无则创建）
	if feishuAppID != "" || feishuSecret != "" {
		if len(cfg.Platforms.Feishu) == 0 {
			cfg.Platforms.Feishu = []config.FeishuConfig{{Name: "feishu"}}
		}
		if feishuAppID != "" {
			cfg.Platforms.Feishu[0].Enabled = true
			cfg.Platforms.Feishu[0].AppID = feishuAppID
		}
		if feishuSecret != "" {
			cfg.Platforms.Feishu[0].AppSecret = feishuSecret
		}
	}
	if telegramToken != "" {
		if len(cfg.Platforms.Telegram) == 0 {
			cfg.Platforms.Telegram = []config.TelegramConfig{{Name: "telegram"}}
		}
		cfg.Platforms.Telegram[0].Enabled = true
		cfg.Platforms.Telegram[0].Token = telegramToken
	}

	fmt.Println()
	fmt.Println("  🦀 HexClaw — AI Agent Engine")
	fmt.Println("  自研引擎 · 多 Agent 协作 · 本地部署 · 数据私有")
	fmt.Println("  ══════════════════════════════════════════════")
	fmt.Printf("  Version:  %s (%s)\n", version, commit)
	fmt.Printf("  Built:    %s\n", date)
	fmt.Printf("  Engine:   Hexagon (ReAct · Tool 调度 · 声明式编排)\n")
	fmt.Printf("  Listen:   %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("  LLM:      %s\n", cfg.LLM.Default)
	fmt.Printf("  PID:      %d\n", os.Getpid())
	if desktopMode {
		fmt.Println("  Mode:     desktop (localhost only, anonymous)")
	}
	fmt.Println("  ──────────────────────────────────────────────")

	// 2. 初始化存储
	store, err := sqlitestore.New(cfg.Storage.SQLite.Path)
	if err != nil {
		return fmt.Errorf("初始化存储失败: %w", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		return fmt.Errorf("初始化数据库表失败: %w", err)
	}
	fmt.Println("  ✓ Storage     SQLite")

	// 3. 初始化 LLM 路由（允许无 Provider 降级运行）
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		fmt.Printf("  ✗ LLM         跳过 (%v)\n", err)
	} else {
		fmt.Printf("  ✓ LLM         %v\n", router.Providers())
	}

	// 4. 初始化 Skill 注册中心
	skills := skill.NewRegistry()
	builtin.RegisterAll(skills, cfg.Skill.Builtin)
	builtinCount := len(skills.All())

	// 4.5 加载 Markdown 技能（技能市场）
	var mp *marketplace.Marketplace
	mdCount := 0
	if cfg.Skills.Enabled {
		mp = marketplace.NewMarketplace(cfg.Skills.Dir)
		if err := mp.Init(); err != nil {
			// 静默，后面统一报告
		} else {
			for _, mdSkill := range mp.List() {
				w := marketplace.WrapAsSkill(mdSkill)
				if err := skills.Register(w); err == nil {
					_ = skills.SetEnabled(mdSkill.Meta.Name, mp.IsEnabled(mdSkill.Meta.Name))
					mdCount++
				}
			}
		}
	}
	if mdCount > 0 {
		fmt.Printf("  ✓ Skills      %d 内置 + %d Markdown\n", builtinCount, mdCount)
	} else {
		fmt.Printf("  ✓ Skills      %d 内置\n", builtinCount)
	}

	// 4.6 连接 MCP Server
	var mcpMgr *hexmcp.Manager
	if cfg.MCP.Enabled && len(cfg.MCP.Servers) > 0 {
		mcpMgr = hexmcp.NewManager()
		var mcpConfigs []hexmcp.ServerConfig
		for _, s := range cfg.MCP.Servers {
			enabled := s.Enabled
			if !enabled && (s.Command != "" || s.Endpoint != "") {
				enabled = true
			}
			mcpConfigs = append(mcpConfigs, hexmcp.ServerConfig{
				Name:      s.Name,
				Transport: s.Transport,
				Command:   s.Command,
				Args:      s.Args,
				Endpoint:  s.Endpoint,
				Enabled:   enabled,
			})
		}
		totalTools, err := mcpMgr.Connect(ctx, mcpConfigs)
		if err != nil {
			fmt.Printf("  ✗ MCP         连接出错: %v\n", err)
		}
		if totalTools > 0 {
			fmt.Printf("  ✓ MCP         %d 个工具 (%d Server)\n", totalTools, len(mcpMgr.ServerNames()))
		} else if err == nil {
			fmt.Println("  ✗ MCP         未连接")
		}
		defer mcpMgr.Close()
	}

	// 5. 初始化安全网关
	gw := gateway.NewPipeline(&cfg.Security, store)
	gwLayers := gw.LayerNames()
	fmt.Printf("  ✓ Gateway     %d 层 (%s)\n", len(gwLayers), strings.Join(gwLayers, " → "))

	// 6. 创建并启动 Agent 引擎
	eng := engine.NewReActEngine(cfg, router, store, skills)

	// 6.5 初始化知识库（向量搜索 + FTS5 混合检索）
	//
	// 分层架构:
	//   embedder: ai-core OpenAI Provider → hexagon rag/embedder 包装
	//   splitter: hexagon rag/splitter.RecursiveSplitter
	//   store:    hexclaw SQLite (文档元数据 + FTS5 + 向量 BLOB)
	kbOK := false
	if cfg.Knowledge.Enabled {
		kbStore := knowledge.NewSQLiteStore(store.DB())
		if err := kbStore.Init(ctx); err == nil {
			// 1. 构造 embedder: ai-core Provider → hexagon embedder 包装
			var emb *ragembedder.OpenAIEmbedder
			if cfg.Knowledge.Embedding.Provider != "" {
				providerName := cfg.Knowledge.Embedding.Provider
				if pc, ok := cfg.LLM.Providers[providerName]; ok && pc.APIKey != "" {
					model := cfg.Knowledge.Embedding.Model
					if model == "" {
						model = "text-embedding-3-small"
					}
					var providerOpts []aiopenai.Option
					if pc.BaseURL != "" {
						providerOpts = append(providerOpts, aiopenai.WithBaseURL(pc.BaseURL))
					}
					aiProvider := aiopenai.New(pc.APIKey, providerOpts...)
					dim := aiopenai.EmbeddingDimension(model)
					emb = ragembedder.NewOpenAIEmbedder(aiProvider,
						ragembedder.WithModel(model),
						ragembedder.WithDimension(dim),
					)
				}
			}

			// 2. 构造 splitter: hexagon RecursiveSplitter
			chunkSize := cfg.Knowledge.ChunkSize
			if chunkSize <= 0 {
				chunkSize = 400
			}
			chunkOverlap := cfg.Knowledge.ChunkOverlap
			if chunkOverlap <= 0 {
				chunkOverlap = 80
			}
			sp := splitter.NewRecursiveSplitter(
				splitter.WithRecursiveChunkSize(chunkSize),
				splitter.WithRecursiveChunkOverlap(chunkOverlap),
			)

			// 3. 混合检索配置
			hybridCfg := knowledge.DefaultHybridConfig()
			if cfg.Knowledge.VectorWeight > 0 {
				hybridCfg.VectorWeight = cfg.Knowledge.VectorWeight
			}
			if cfg.Knowledge.TextWeight > 0 {
				hybridCfg.TextWeight = cfg.Knowledge.TextWeight
			}
			if cfg.Knowledge.MMRLambda > 0 {
				hybridCfg.MMRLambda = cfg.Knowledge.MMRLambda
			}
			hybridCfg.TimeDecayDays = cfg.Knowledge.TimeDecayDays

			// 4. 创建 Manager (kbStore 同时实现 DocumentRepository + ChunkSearcher)
			kbMgr := knowledge.NewManager(kbStore, kbStore, emb,
				knowledge.WithSplitter(sp),
				knowledge.WithHybridConfig(hybridCfg),
			)
			eng.SetKnowledgeBase(kbMgr)
			kbOK = true
		}
	}
	if kbOK {
		fmt.Println("  ✓ Knowledge   FTS5 + 向量混合检索 (hexagon RAG)")
	} else {
		fmt.Println("  ✗ Knowledge   未启用")
	}

	// 6.6 从 SQLite 恢复 LLM 缓存（减少冷启动后的 API 开销）
	eng.LLMCache().LoadFromDB(store.DB())

	// 7. 初始化文件记忆系统
	var fileMem *memory.FileMemory
	var memCtxLen int
	if cfg.FileMemory.Enabled {
		var err error
		fileMem, err = memory.New(memory.Options{
			Enabled:   true,
			Dir:       cfg.FileMemory.Dir,
			MaxMemory: cfg.FileMemory.MaxMemory,
			DailyDays: cfg.FileMemory.DailyDays,
		})
		if err == nil {
			memCtx := fileMem.LoadContext()
			memCtxLen = len(memCtx)
		}
	}
	if memCtxLen > 0 {
		fmt.Printf("  ✓ Memory      文件记忆 (%d 字符)\n", memCtxLen)
	} else {
		fmt.Println("  ✗ Memory      未启用")
	}

	if err := eng.Start(ctx); err != nil {
		return fmt.Errorf("启动引擎失败: %w", err)
	}
	defer eng.Stop(context.Background())

	// 8. 启动 HTTP 服务
	srv := api.NewServer(cfg, eng, gw, store)
	srv.SetVersion(version)
	lc := srv.LogCollector()

	// 桥接 Go 标准 log 到 LogCollector（让 log.Printf 的输出也进入日志系统）
	log.SetOutput(lc.StdLogWriter())

	// 写入启动摘要日志
	lc.Info("system", fmt.Sprintf("🦀 HexClaw %s 启动 — 自研引擎 · 多 Agent 协作 · 本地部署 · 数据私有", version))
	lc.Info("system", fmt.Sprintf("Engine: Hexagon (ReAct · Tool 调度 · 声明式编排) | PID: %d", os.Getpid()))
	lc.Info("system", fmt.Sprintf("Listen: %s:%d | LLM: %s", cfg.Server.Host, cfg.Server.Port, cfg.LLM.Default))
	if desktopMode {
		lc.Info("system", "Mode: desktop — Sidecar 架构，零云端依赖，数据完全私有")
	}
	lc.Info("storage", "SQLite 已初始化 — 会话持久化就绪")
	if router != nil {
		lc.Info("llm", fmt.Sprintf("LLM Providers: %v — 多模型统一适配，运行时热切换", router.Providers()))
	}
	lc.Info("gateway", fmt.Sprintf("安全网关 %d 层: %s", len(gwLayers), strings.Join(gwLayers, " → ")))
	lc.Info("skills", fmt.Sprintf("Skills: %d 内置 — 搜索/天气/翻译/摘要等开箱即用", builtinCount))
	if kbOK {
		lc.Info("knowledge", "知识库已启用 — FTS5 + 向量混合检索，RAG 增强问答")
	}
	if memCtxLen > 0 {
		lc.Info("memory", fmt.Sprintf("文件记忆已加载 (%d 字符) — 跨会话长期记忆", memCtxLen))
	}
	lc.Info("system", fmt.Sprintf("Web UI: http://%s:%d | Chat API: POST /api/v1/chat", cfg.Server.Host, cfg.Server.Port))
	lc.Info("system", "🦀 HexClaw 已就绪 — 数据全在本地，横行无忧")

	// 挂载知识库 API
	if eng.KnowledgeBase() != nil {
		srv.SetKnowledgeBase(eng.KnowledgeBase())
	}

	// 9. 初始化定时任务调度器
	var scheduler *cron.Scheduler
	cronOK := false
	if cfg.Cron.Enabled {
		scheduler = cron.NewScheduler(store.DB())
		if err := scheduler.Init(ctx); err != nil {
			scheduler = nil
		} else {
			scheduler.Start(ctx, func(ctx context.Context, job *cron.Job) (string, error) {
				reply, err := eng.Process(ctx, &adapter.Message{
					Platform: adapter.PlatformAPI,
					UserID:   job.UserID,
					Content:  job.Prompt,
					Metadata: map[string]string{"source": "cron", "job_id": job.ID},
				})
				if err != nil {
					return "", err
				}
				if reply != nil {
					return reply.Content, nil
				}
				return "", nil
			})
			cronOK = true
		}
	}
	if cronOK {
		fmt.Println("  ✓ Cron        调度器已启动")
	} else {
		fmt.Println("  ✗ Cron        未启用")
	}

	// 10. 初始化 Webhook 管理器
	var webhookMgr *webhook.Manager
	webhookOK := false
	if cfg.Webhook.Enabled {
		webhookMgr = webhook.NewManager(store.DB())
		if err := webhookMgr.Init(ctx); err != nil {
			webhookMgr = nil
		} else {
			webhookMgr.SetHandler(func(ctx context.Context, event *webhook.Event, prompt string) error {
				content := fmt.Sprintf("[Webhook: %s] %s\n\n指令: %s\n\nPayload 摘要: %s",
					event.WebhookName, event.EventType, prompt, event.Summary)
				_, err := eng.Process(ctx, &adapter.Message{
					Platform: adapter.PlatformAPI,
					UserID:   "webhook-system",
					Content:  content,
					Metadata: map[string]string{"source": "webhook", "webhook": event.WebhookName},
				})
				return err
			})
			srv.SetWebhookManager(webhookMgr)
			webhookOK = true
		}
	}
	if webhookOK {
		fmt.Println("  ✓ Webhook     已启用")
	} else {
		fmt.Println("  ✗ Webhook     未启用")
	}

	// 11. 初始化心跳巡查
	var hb *heartbeat.Heartbeat
	if cfg.Heartbeat.Enabled {
		intervalMins := cfg.Heartbeat.IntervalMins
		if intervalMins <= 0 {
			intervalMins = 15
		}

		hbCfg := heartbeat.Config{
			Enabled:      true,
			Interval:     time.Duration(intervalMins) * time.Minute,
			Instructions: cfg.Heartbeat.Instructions,
			QuietHours: heartbeat.QuietHours{
				Enabled: cfg.Heartbeat.QuietStart != "" && cfg.Heartbeat.QuietEnd != "",
				Start:   cfg.Heartbeat.QuietStart,
				End:     cfg.Heartbeat.QuietEnd,
			},
		}

		hb = heartbeat.New(hbCfg)
		executor := func(ctx context.Context, instructions string) (string, error) {
			reply, err := eng.Process(ctx, &adapter.Message{
				Platform: adapter.PlatformAPI,
				UserID:   "heartbeat-system",
				Content:  instructions,
				Metadata: map[string]string{"source": "heartbeat"},
			})
			if err != nil {
				return "", err
			}
			return reply.Content, nil
		}
		notifier := func(ctx context.Context, message string) error {
			lc.Info("heartbeat", message)
			return nil
		}
		hb.Start(ctx, executor, notifier)
		fmt.Printf("  ✓ Heartbeat   每 %d 分钟巡查\n", intervalMins)
	} else {
		fmt.Println("  ✗ Heartbeat   未启用")
	}

	// 挂载 Phase 3 API 端点
	if scheduler != nil {
		srv.SetCronScheduler(scheduler)
	}
	if fileMem != nil {
		srv.SetFileMemory(fileMem)
	}

	// 挂载 Phase 4 API 端点
	if mcpMgr != nil {
		srv.SetMCPManager(mcpMgr)
	}
	if mp != nil {
		srv.SetMarketplace(mp)
	}

	// 12. 初始化多 Agent 路由 + 持久化
	agentRouter := agentrouter.New()
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(ctx); err != nil {
		log.Printf("Agent 存储初始化失败: %v", err)
	}

	// 先从 DB 加载已持久化的 Agent 和规则
	agents, defaultName, _ := agentStore.LoadAgents(ctx)
	rules, _ := agentStore.LoadRules(ctx)

	// 如果 DB 为空，从配置文件种子数据初始化
	if len(agents) == 0 && len(cfg.Router.Agents) > 0 {
		for _, ac := range cfg.Router.Agents {
			agents = append(agents, agentrouter.AgentConfig{
				Name:         ac.Name,
				DisplayName:  ac.DisplayName,
				Description:  ac.Description,
				Model:        ac.Model,
				Provider:     ac.Provider,
				SystemPrompt: ac.SystemPrompt,
				Skills:       ac.Skills,
				MaxTokens:    ac.MaxTokens,
				Temperature:  ac.Temperature,
				Metadata:     ac.Metadata,
			})
		}
		for _, rc := range cfg.Router.Rules {
			rules = append(rules, agentrouter.Rule{
				Platform:   rc.Platform,
				InstanceID: rc.InstanceID,
				UserID:     rc.UserID,
				ChatID:     rc.ChatID,
				AgentName:  rc.AgentName,
				Priority:   rc.Priority,
			})
		}
		if cfg.Router.DefaultAgent != "" {
			defaultName = cfg.Router.DefaultAgent
		}
	}

	agentRouter.LoadAll(agents, defaultName, rules)

	// 配置种子写入 DB（幂等）
	if len(agents) > 0 {
		_ = agentrouter.Sync(ctx, agentStore, agentRouter)
	}

	// LLM 语义路由 fallback：规则不命中时用 LLM 分类
	if cfg.Router.LLMFallback && router != nil {
		classifier := agentrouter.NewLLMClassifier(func(ctx context.Context, systemPrompt, userMessage string) (string, error) {
			provider, _, err := router.Route(ctx)
			if err != nil {
				return "", err
			}
			resp, err := provider.Complete(ctx, hexagon.CompletionRequest{
				Messages: []hexagon.Message{
					{Role: hexagon.RoleSystem, Content: systemPrompt},
					{Role: hexagon.RoleUser, Content: userMessage},
				},
				MaxTokens: 64,
			})
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		})
		agentRouter.SetClassifier(classifier)
	}

	eng.SetAgentRouter(agentRouter)
	srv.SetAgentRouter(agentRouter)
	srv.SetAgentStore(agentStore)

	agentCount := len(agentRouter.ListAgents())
	ruleCount := len(agentRouter.ListRules())
	if agentCount > 0 {
		fmt.Printf("  ✓ Agents      %d 个 Agent, %d 条规则", agentCount, ruleCount)
		if cfg.Router.LLMFallback {
			fmt.Print(" + LLM fallback")
		}
		fmt.Println()
	} else {
		fmt.Println("  ✓ Agents      多 Agent 路由（空）")
	}

	// 13. 初始化 Canvas/A2UI 服务（Phase 5）
	var canvasSvc *canvas.Service
	if cfg.Canvas.Enabled {
		canvasSvc = canvas.NewService()
		srv.SetCanvas(canvasSvc)
		fmt.Println("  ✓ Canvas      A2UI")
	} else {
		fmt.Println("  ✗ Canvas      未启用")
	}

	// 14. 初始化语音服务（Phase 5）
	var voiceSvc *voice.Service
	if cfg.Voice.Enabled {
		var stt voice.STTProvider
		var tts voice.TTSProvider

		if cfg.Voice.STT.Provider != "" {
			llmName := extractLLMName(cfg.Voice.STT.Provider)
			if pc, ok := cfg.LLM.Providers[llmName]; ok && pc.APIKey != "" {
				var sttOpts []voice.STTOption
				if pc.BaseURL != "" {
					sttOpts = append(sttOpts, voice.STTWithBaseURL(pc.BaseURL))
				}
				stt = voice.NewOpenAISTT(pc.APIKey, cfg.Voice.STT.Model, sttOpts...)
			}
		}

		if cfg.Voice.TTS.Provider != "" {
			llmName := extractLLMName(cfg.Voice.TTS.Provider)
			if pc, ok := cfg.LLM.Providers[llmName]; ok && pc.APIKey != "" {
				var ttsOpts []voice.TTSOption
				if pc.BaseURL != "" {
					ttsOpts = append(ttsOpts, voice.TTSWithBaseURL(pc.BaseURL))
				}
				tts = voice.NewOpenAITTS(pc.APIKey, "", ttsOpts...)
			}
		}

		voiceSvc = voice.NewService(stt, tts)
		srv.SetVoice(voiceSvc)
		fmt.Printf("  ✓ Voice       STT=%s, TTS=%s\n", voiceSvc.STTName(), voiceSvc.TTSName())
	} else {
		fmt.Println("  ✗ Voice       未启用")
	}

	// 15. 初始化桌面集成服务（Phase 6）
	desktopSvc := desktop.NewService(version)
	srv.SetDesktop(desktopSvc)

	// 抑制未使用变量警告
	_ = agentRouter
	_ = canvasSvc
	_ = voiceSvc

	messageHandler := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		if err := gw.Check(ctx, msg); err != nil {
			return &adapter.Reply{Content: "安全检查未通过: " + err.Error()}, nil
		}
		return eng.Process(ctx, msg)
	}

	instanceMgr := instances.NewManager(store.DB())
	if err := instanceMgr.Init(ctx); err != nil {
		return fmt.Errorf("初始化平台实例运行时失败: %w", err)
	}
	if err := instanceMgr.SeedFromConfig(ctx, cfg); err != nil {
		return fmt.Errorf("写入平台实例种子失败: %w", err)
	}
	instanceMgr.SetHandler(messageHandler)
	srv.SetInstanceManager(instanceMgr)

	// Web WebSocket 适配器
	if cfg.Platforms.Web.Enabled {
		wa := webadapter.New()
		if err := wa.Start(ctx, messageHandler); err != nil {
			fmt.Printf("  ✗ Adapter     Web 启动失败: %v\n", err)
		} else {
			wa.SetStreamHandler(func(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
				if err := gw.Check(ctx, msg); err != nil {
					return nil, fmt.Errorf("安全检查未通过: %w", err)
				}
				return eng.ProcessStream(ctx, msg)
			})
			srv.SetWebSocketHandler(wa.Handler())
		}
	}
	if err := instanceMgr.StartEnabled(ctx); err != nil {
		return fmt.Errorf("启动平台实例失败: %w", err)
	}

	// 监听退出信号，优雅关闭
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(sigCtx); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// 适配器列表
	if instanceList, err := instanceMgr.List(ctx); err == nil && len(instanceList) > 0 {
		var names []string
		for _, inst := range instanceList {
			if inst.Enabled {
				names = append(names, inst.Name)
			}
		}
		if len(names) > 0 {
			fmt.Printf("  ✓ Adapters    %s\n", strings.Join(names, ", "))
		}
	}

	fmt.Println("  ──────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  🦀 HexClaw 已就绪 — 数据全在本地，横行无忧")
	fmt.Println()
	fmt.Printf("    Web UI:   http://%s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("    Health:   http://%s:%d/health\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("    Chat:     POST http://%s:%d/api/v1/chat\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Println()
	fmt.Println("  ══════════════════════════════════════════════")
	fmt.Println()

	// 等待退出信号或服务器错误
	select {
	case <-sigCtx.Done():
		fmt.Println("\n  🦀 收到退出信号，正在关闭...")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("服务器异常: %w", err)
		}
	}

	// 优雅关闭（30 秒超时，防止永久阻塞）
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// 先停止心跳和定时任务，再关闭 HTTP 服务
	if hb != nil {
		hb.Stop()
	}
	if scheduler != nil {
		scheduler.Stop()
	}

	// 持久化 LLM 缓存到 SQLite（下次启动恢复）
	eng.LLMCache().PersistToDB(store.DB())

	if err := srv.Stop(shutdownCtx); err != nil {
		log.Printf("关闭服务器出错: %v", err)
	}

	if err := instanceMgr.StopAll(shutdownCtx); err != nil {
		log.Printf("关闭平台实例出错: %v", err)
	}

	fmt.Println("  🦀 HexClaw 已停止")
	return nil
}

// runInit 初始化配置
func runInit() error {
	path, err := config.Init()
	if err != nil {
		return fmt.Errorf("初始化配置失败: %w", err)
	}
	fmt.Printf("配置文件已生成: %s\n", path)
	return nil
}

// newSecurityCmd 创建 security 子命令组
func newSecurityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "security",
		Short: "安全相关命令",
	}

	var configFile string

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "执行安全审计",
		Long: `一键安全检查，涵盖配置安全、网络暴露、工具权限、凭证泄露等维度。

审计结果按严重等级分类：
  Critical: 必须立即修复
  High:     强烈建议修复
  Medium:   建议修复
  Low:      可选优化`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}

			checker := audit.NewChecker(cfg)
			report := checker.Run()
			fmt.Print(report.Summary())

			if report.HasCritical() {
				return fmt.Errorf("发现 %d 个 Critical 级别安全问题", report.CountBySeverity(audit.SeverityCritical))
			}
			return nil
		},
	}
	auditCmd.Flags().StringVar(&configFile, "config", "", "配置文件路径")

	cmd.AddCommand(auditCmd)
	return cmd
}

// extractLLMName 从 Provider 名称中提取 LLM Provider 名
//
// 语音 Provider 名称通常包含 LLM 前缀（如 "openai-whisper"、"openai-tts"），
// 需要提取 LLM 名称（如 "openai"）以从配置中查找对应的 API Key。
//
// 示例:
//   - "openai-whisper" → "openai"
//   - "openai-tts" → "openai"
//   - "openai" → "openai"
//   - "deepseek-stt" → "deepseek"
func extractLLMName(providerName string) string {
	parts := strings.SplitN(providerName, "-", 2)
	return parts[0]
}
