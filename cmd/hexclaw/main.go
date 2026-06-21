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
	"log/slog"

	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/spf13/cobra"

	imagegen "github.com/hexagon-codes/ai-core/media/image"
	videogen "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/ai-core/media/voice"
	"github.com/hexagon-codes/ai-core/media/voicechat"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/events"
	"github.com/hexagon-codes/hexagon/observe/trace"
	genstore "github.com/hexagon-codes/toolkit/blobstore"

	"github.com/hexagon-codes/hexclaw/adapter"
	webadapter "github.com/hexagon-codes/hexclaw/adapter/web"
	"github.com/hexagon-codes/hexclaw/agents"
	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/audit"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/connector"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/desktop"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/gateway"
	"github.com/hexagon-codes/hexclaw/heartbeat"
	"github.com/hexagon-codes/hexclaw/instances"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/library"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/render"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/secret"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/hexclaw/webhook"
)

// 版本信息，通过 -ldflags 注入
var (
	version = "v0.4.4"
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
		newMCPCmd(),
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
// applyDesktopOverrides 桌面客户端模式的配置覆盖：仅监听 localhost、
// 启用 WebSocket、跳过认证，并打开 UI 提供入口的本地功能（Cron / Canvas / Webhook）。
func applyDesktopOverrides(cfg *config.Config) {
	cfg.Server.Host = "127.0.0.1"
	cfg.Platforms.Web.Enabled = true
	cfg.Security.Auth.AllowAnonymous = true
	cfg.Cron.Enabled = true
	cfg.Canvas.Enabled = true
	cfg.Webhook.Enabled = true
}

func runServe(configFile, feishuAppID, feishuSecret, telegramToken string, desktopMode bool) error {
	// 1. 加载配置
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 1.5 桌面端单实例锁：避免重复启动 / stale 进程占端口。
	// 服务端模式（desktopMode=false）通常用容器编排，无需 lock。
	var sidecarLock *SidecarLock
	if desktopMode {
		home, _ := os.UserHomeDir()
		l, lockErr := AcquireSidecarLock(home)
		if lockErr != nil {
			return fmt.Errorf("启动失败: %w", lockErr)
		}
		sidecarLock = l
		defer sidecarLock.Release()
	}

	if desktopMode {
		applyDesktopOverrides(cfg)
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

	// 2. 初始化存储（含健康检查）
	checkDBHealth(cfg.Storage.SQLite.Path)
	store, err := sqlitestore.New(cfg.Storage.SQLite.Path)
	if err != nil {
		return fmt.Errorf("初始化存储失败: %w", err)
	}
	defer store.Close()

	// v0.4.0 方案 A：构造 Feature Flag Static 实例并注入 root ctx。
	// 所有 P1 重构项（Agent factory v2 / Skill 7 阶段 / MCP v2 等）必须挂 flag，
	// 默认 OFF；用户在 hexclaw.yaml 的 features: 段或 Settings 显式开启。
	// 业务路径用 featureflag.Enabled(ctx, "name") 查询，fail-closed。
	flags := featureflag.NewStatic(featureflag.Registered(), cfg.Features)
	ctx := featureflag.WithContext(context.Background(), flags)
	if registered := featureflag.Registered(); len(registered) > 0 {
		fmt.Printf("  ✓ Features    %d 个 flag 注册（其中 %d 启用）\n",
			len(registered), countEnabledFlags(flags))
	}

	// v0.4.0 H6 接入：注入 Emitter 到 root ctx，供 tool/llm/mcp 关键点埋的 Emit 使用。
	//
	// v0.5.0：events 下沉到框架层 hexagon/observe/events，去除 feature flag
	// (events.transport.v1)。去 flag 后默认静默改由 Sink 选择控制 —— 这里默认用
	// NoopSink（等价于原 flag OFF 的"默认静默"，避免长驻进程 MemorySink 无界增长）；
	// 需要观测时在此切到 FileSink / HTTPSink / MultiSink。
	emitter := events.NewEmitter(events.NewNoopSink(), "hexclaw")
	ctx = events.WithEmitter(ctx, emitter)

	// v0.4.0 F8 接入：注入全局 ChainPricer —— flag pricing.layered.v1 OFF 时
	// EstimateCost 仍走老 pricingTable；flag ON 时优先 user override → builtin。
	engine.SetGlobalPricer(engine.NewDefaultPricer(
		engine.NewUserOverridePricer(nil),
		nil, // RemoteFetcher 留待 v0.4.x 后续接 openrouter
		time.Hour,
	))
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

	// 4.6 连接 MCP Server（即使无预配 Server 也初始化 Manager，支持动态添加）
	var mcpMgr *hexmcp.Manager
	if cfg.MCP.Enabled {
		mcpMgr = hexmcp.NewManager()
		defer mcpMgr.Close()
		if len(cfg.MCP.Servers) > 0 {
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
		}
	}

	// 4.7 注册高级 Skill (需依赖注入: sandbox/hub/mcp)
	skillDeps := builtin.SkillDeps{
		McpMgr: mcpMgr,
	}
	builtin.RegisterAdvanced(skills, cfg.Skill.Builtin, &skillDeps)
	advCount := len(skills.All()) - builtinCount - mdCount
	if advCount > 0 {
		fmt.Printf("  ✓ Advanced    %d 个高级 Skill (SkillWriter/Installer/FileOps)\n", advCount)
	}

	// v0.4.0 H3：MCP Manager 内部的 lifecycle hook 自取 ctx 中的 flags，
	// 所以这里无需额外注入；mcpMgr 仅作为引用保留供下游使用。
	//
	// v0.4.0 H5 plugin Manager 当前未在 main 实例化 —— 协议（plugin/extension.go）
	// 已就绪，待 v0.4.x 第三方 plugin 真接入时再由那时的入口调用 plugin.NewManager
	// 并 SetHostContext("0.4.x", flags)。本次不引入空 Manager 避免误导。
	_ = mcpMgr

	// 5. 初始化安全网关
	gw := gateway.NewPipeline(&cfg.Security, store)
	gwLayers := gw.LayerNames()
	fmt.Printf("  ✓ Gateway     %d 层 (%s)\n", len(gwLayers), strings.Join(gwLayers, " → "))

	// 6. 创建并启动 Agent 引擎
	eng := engine.NewReActEngine(cfg, router, store, skills)

	// v0.4.0 H8 接入：注入默认 ObserveMiddleware → events.Emitter。
	// flag model.gateway.v1 OFF 时 Chain 自动 no-op；flag ON 时每次 Provider
	// Complete/Stream 都会投递 "llm.call.observed" 结构化事件到 emitter sink。
	eng.SetDefaultLLMMiddlewares([]engine.ProviderMiddleware{
		engine.ObserveMiddleware(engine.NewEventsRecorder(emitter, "engine.llm_observe")),
	})

	// 6.1 接入统一工具循环 (D1-D3 产出)
	toolCollector := engine.NewToolCollector(skills, mcpMgr, 40)
	toolExecutor := engine.NewToolExecutor(skills, mcpMgr)
	toolExecutor.AddHook(&engine.AuditHook{})
	toolExecutor.AddHook(&engine.TruncateHook{MaxChars: 8000})
	toolExecutor.AddHook(&engine.SanitizeHook{}) // Phase 9: prompt injection defense

	// 6.1.1 接入权限审批 Hook (D24)
	// v0.4.3 §11.10 统一安全闸：PermissionPolicy 为单一权限闸（GA 默认 ON）。无人值守
	// 风险顾问注入 —— cron/webhook 等无交互会话下，命中 require_approval 的 consequential
	// 动作改问 LLM 判级（eng.JudgeText），仅 low 放行一次，否则 fail-closed 拒。
	permHub := engine.NewPermissionHub(60 * time.Second)
	permHook := engine.NewPermissionHook(permHub,
		engine.WithCodeExecApproval(cfg.Skill.Builtin.CodeExecPolicy.CodeExecRequiresApproval()),
		engine.WithPolicy(engine.DefaultBaselinePolicy()),
		engine.WithUnattendedReviewer(unattendedRiskAdapter{builtin.NewLLMRiskReviewer(eng.JudgeText)}),
	)
	toolExecutor.AddHook(permHook)

	// 6.1.2 接入 per-tool 权限控制 (Phase 9 D40)
	if len(cfg.Security.ToolPermissions.Deny) > 0 || len(cfg.Security.ToolPermissions.Allow) > 0 {
		perms := engine.NewToolPermissions(cfg.Security.ToolPermissions.Allow, cfg.Security.ToolPermissions.Deny)
		toolExecutor.AddHook(engine.NewToolPermissionHook(perms))
	}

	// 6.1.3 v0.4.x C3 自改进：ImproveStore + ImproveHook + LLM-as-judge
	// draft 写到 <skills-dir>/.drafts/，用户审核后手动 mv 到 skills/。
	// Judge 用 default provider 采样评分（10% 概率走 LLM，避免每次工具调用都消耗配额）。
	userHome, _ := os.UserHomeDir()
	skillDraftDir := computeSkillDraftDir(cfg.Skills.Dir, userHome)
	improveStore := skill.NewImproveStore(skillDraftDir)
	if defaultProv := router.Default(); defaultProv != nil {
		improveStore.Judge = engine.NewLLMSkillJudge(
			defaultProv,
			router.ProviderModel(router.DefaultName()),
			engine.LLMSkillJudgeOptions{SampleRate: 0.1},
		)
	}
	if hook := engine.NewImproveHook(improveStore); hook != nil {
		toolExecutor.AddHook(hook)
	}

	eng.SetToolCollector(toolCollector)
	eng.SetToolExecutor(toolExecutor)

	// v0.4.0 H1 闭环：启动时调一次 InitLifecycle，让所有实现 LifecycleTool 接口的 Skill
	// 在请求到达前完成一次性资源初始化（数据库连接 / 索引预热 / 远程 token 预换）。
	// flag tool.lifecycle.v2 OFF 时本调用是 no-op，不影响老路径。
	if err := toolExecutor.InitLifecycle(ctx); err != nil {
		fmt.Printf("  ⚠ Lifecycle init 失败：%v（继续启动，受影响的 Skill 可能首次调用变慢）\n", err)
	}

	// v0.4.0 G1 接入：构造 HexagonDispatcher 让 engine 可按 metadata.dispatch_role
	// 路由到 hexagon.Agent。flag agent.factory.real OFF 时 Dispatch 立即返回
	// ErrDispatchDisabled，调用方走 ReAct 老路径。
	if router != nil && router.Default() != nil {
		factory := agents.NewFactory()
		eng.SetHexagonDispatcher(agents.NewHexagonDispatcher(factory, router.Default()))
	}
	eng.SetSessionLock(session.NewSessionLock())
	fmt.Printf("  ✓ Tools       %d 个工具 (Skill + MCP)\n", len(toolCollector.Collect()))

	// 6.2 接入预算控制器 (G1: 三维预算 - token/duration/cost)
	budgetDuration := 30 * time.Minute
	if cfg.Budget.MaxDuration != "" {
		if d, err := time.ParseDuration(cfg.Budget.MaxDuration); err == nil {
			budgetDuration = d
		}
	}
	budgetCtrl := engine.NewBudgetController(engine.BudgetConfig{
		MaxTokens:   cfg.Budget.MaxTokens,
		MaxDuration: budgetDuration,
		MaxCost:     cfg.Budget.MaxCost,
	})
	eng.SetBudget(budgetCtrl)
	fmt.Printf("  ✓ Budget      max_tokens=%d, max_duration=%v, max_cost=$%.2f\n",
		cfg.Budget.MaxTokens, budgetDuration, cfg.Budget.MaxCost)

	// 6.5 初始化知识库（向量搜索 + FTS5 混合检索）
	//
	// 分层架构:
	//   embedder: ai-core OpenAI Provider → hexagon rag/embedder 包装
	//   splitter: hexagon rag/splitter.RecursiveSplitter
	//   store:    hexclaw SQLite (文档元数据 + FTS5 + 向量 BLOB)
	kbOK := false
	var sharedEmbedder hexagon.VectorEmbedder // 共享 embedder: KB + VectorMemory + 语义搜索
	if cfg.Knowledge.Enabled {
		kbStore := knowledge.NewSQLiteStore(store.DB())
		if err := kbStore.Init(ctx); err == nil {
			// 1. 构造 embedder: ai-core Provider → hexagon embedder 包装
			var emb *hexagon.OpenAIEmbedder

			// 确定 embedding provider：显式配置 > 自动检测（Ollama nomic-embed-text > 有 API Key 的云服务）
			embProviderName := cfg.Knowledge.Embedding.Provider
			embModel := cfg.Knowledge.Embedding.Model

			if embProviderName == "" {
				// 自动检测：优先 Ollama（本地、免费、无需 API Key）
				for name, pc := range cfg.LLM.Providers {
					lower := strings.ToLower(name)
					isOllamaProvider := strings.Contains(lower, "ollama") || strings.Contains(strings.ToLower(pc.BaseURL), "localhost:11434")
					if isOllamaProvider {
						embProviderName = name
						if embModel == "" {
							embModel = "nomic-embed-text"
						}
						logger.Info("[knowledge] 自动选择 Ollama 作为 embedding provider", "name", name, "model", embModel)
						break
					}
				}
			}
			if embProviderName == "" {
				// 其次：找第一个有 API Key 的云 provider
				for name, pc := range cfg.LLM.Providers {
					if pc.APIKey != "" {
						embProviderName = name
						logger.Info("[knowledge] 自动选择云服务作为 embedding provider", "name", name)
						break
					}
				}
			}

			if embProviderName != "" {
				if pc, ok := cfg.LLM.Providers[embProviderName]; ok {
					if embModel == "" {
						embModel = "text-embedding-3-small"
					}
					// Ollama 不需要 API Key，云服务需要
					isOllama := strings.ToLower(embProviderName) == "ollama" || strings.Contains(strings.ToLower(pc.BaseURL), "localhost:11434")
					if isOllama || pc.APIKey != "" {
						var providerOpts []hexagon.OpenAIOption
						if pc.BaseURL != "" {
							providerOpts = append(providerOpts, hexagon.OpenAIWithBaseURL(pc.BaseURL))
						}
						apiKey := pc.APIKey
						if apiKey == "" {
							apiKey = "ollama" // Ollama 不需要真实 key，但 OpenAI client 要求非空
						}
						aiProvider := hexagon.NewOpenAI(apiKey, providerOpts...)
						dim := hexagon.OpenAIEmbeddingDimension(embModel)
						emb = hexagon.NewOpenAIEmbedder(aiProvider,
							hexagon.WithEmbedderModel(embModel),
							hexagon.WithEmbedderDimension(dim),
						)
						logger.Info("[knowledge] 自动配置 embedding", "provider", embProviderName, "model", embModel)
					}
				}
			}

			if emb != nil {
				sharedEmbedder = emb // 共享给 VectorMemory 和语义搜索
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
			sp := hexagon.NewRecursiveSplitter(
				hexagon.WithRecursiveChunkSize(chunkSize),
				hexagon.WithRecursiveChunkOverlap(chunkOverlap),
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
			// 注意: 传 sharedEmbedder（接口类型）而非 emb（*OpenAIEmbedder），
			// 避免 Go 接口 nil 陷阱（typed nil pointer 使接口非 nil 但 receiver 为 nil）
			kbMgr := knowledge.NewManager(kbStore, kbStore, sharedEmbedder,
				knowledge.WithSplitter(sp),
				knowledge.WithHybridConfig(hybridCfg),
			)
			eng.SetKnowledgeBase(kbMgr)
			// Register the knowledge_ingest skill: the only channel for the Agent
			// to persist content into the knowledge base. Without it the
			// "collect → ingest" loop is broken (the LLM can only hallucinate success).
			if err := skills.Register(builtin.NewKnowledgeIngestSkill(kbMgr)); err != nil {
				logger.Warn("[knowledge] failed to register knowledge_ingest skill", "err", err.Error())
			}
			// batch/directory ingest: the model passes a PATH and the code reads
			// each file's real body — never a filename listing. Sandboxed to the
			// FileOps workspace via resolveSafePath.
			if err := skills.Register(builtin.NewKnowledgeIngestPathSkill(kbMgr, builtin.DefaultWorkspace())); err != nil {
				logger.Warn("[knowledge] failed to register knowledge_ingest_path skill", "err", err.Error())
			}
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
		fmt.Printf("  ✓ Memory      文件记忆 (%d 字符) + 自动记忆\n", memCtxLen)
		eng.SetFileMemory(fileMem)
	} else {
		fmt.Println("  ✗ Memory      未启用")
	}

	// 7.5 初始化向量语义记忆 (D4: 链路④修复)
	if cfg.Memory.Vector.Enabled && sharedEmbedder != nil {
		vecStore := hexagon.NewMemoryVectorStore(sharedEmbedder.Dimension())
		vecMem := memory.NewVectorMemory(vecStore, sharedEmbedder, memory.VectorMemoryConfig{
			Enabled:  true,
			TopK:     cfg.Memory.Vector.TopK,
			MinScore: float32(cfg.Memory.Vector.MinScore),
			AutoSave: cfg.Memory.Vector.AutoSave,
		})
		eng.SetVectorMemory(vecMem)
		fmt.Println("  ✓ VectorMem   语义记忆 (内存向量库)")
	}

	if err := eng.Start(ctx); err != nil {
		return fmt.Errorf("启动引擎失败: %w", err)
	}
	defer eng.Stop(context.Background())

	// 8. 启动 HTTP 服务
	srv := api.NewServer(cfg, eng, gw, store)
	srv.SetVersion(version)

	// §11.8 交互层：Prompt 库（服务端下发 + CRUD）+ 记忆薄版（standing/fact 每轮注入）。
	promptStore := library.NewPromptStore(store.DB())
	memStore := library.NewMemoryStore(store.DB())
	srv.SetPromptStore(promptStore)
	srv.SetMemoryStore(memStore)
	eng.SetMemoryProvider(memStore) // 每轮组 prompt 时注入 standing 全量 + fact 命中

	// 8.0.1 接入沙箱网络热更新 (Bug2 修复)
	if skillDeps.CodeExecSkill != nil {
		srv.SetSandboxCallbacks(skillDeps.CodeExecSkill.UpdateNetwork, skillDeps.CodeExecSkill.NetworkEnabled)
	}
	lc := srv.LogCollector()

	// 初始化 slog → LogCollector 桥接（结构化日志 + trace ID 贯穿）
	slogHandler := trace.NewCollectorHandler(lc, slog.LevelInfo)
	slog.SetDefault(slog.New(slogHandler))
	// 桥接 Go 标准 log 到 LogCollector（兼容遗留 log.Printf）
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
	// Note: "已就绪" 日志迁移到 onReady 回调，仅在 HTTP 端口真实 bind 成功后才打印。

	// 挂载预算控制器 API
	srv.SetBudgetController(budgetCtrl)

	// 挂载知识库 API
	if eng.KnowledgeBase() != nil {
		srv.SetKnowledgeBase(eng.KnowledgeBase())
	}

	// 挂载 A7 模型 tool_call 能力探测服务
	if router != nil && store != nil {
		srv.SetCapabilityService(llmrouter.NewCapabilityService(router, store))
	}

	// v0.4.0 F9：挂载配置事务热加载 manager（flag config.tx.hotload.v1 OFF 时自动降级到老路径）
	if router != nil {
		txMgr := config.NewTransactionManager(cfg,
			[]config.Validator{config.BuiltinValidator{}},
			[]config.Applier{llmrouter.NewSelectorApplier(router)},
		)
		srv.SetConfigTxManager(txMgr)
	}

	// 9. 初始化定时任务调度器（v2 脚本编译架构）
	//
	// 创建任务时由 LLMCompiler 一次性把用户 prompt 编译成 Python 脚本（JobSpec）；
	// 运行时由 ScriptExecutor 在沙箱中执行，全程零 LLM 调用。
	var scheduler *cron.Scheduler
	cronOK := false
	if cfg.Cron.Enabled {
		if router == nil {
			fmt.Println("  ✗ Cron        未启用 (LLM router 未初始化)")
		} else {
			// 动态 resolver — 每次 Compile 时查最新 default。
			// 用户在 GUI 切 chat 模型立即生效（无需重启），且模型无效时 fail-loud。
			resolver := buildCronProviderResolver(router)
			// 启动期试探一次：仅验证基础可用（不阻塞启动；用户可后续在 UI 改配置）
			if _, model, err := resolver(); err != nil {
				fmt.Printf("  ⚠ Cron        编译 LLM 暂不可用 (%v) — 创建任务时会重新尝试\n", err)
				logger.Warn("[cron] 启动期 resolver 失败", "err", err.Error())
			} else {
				fmt.Printf("  ⓘ Cron        编译模型：%s（远程 chat 优先，对话仍用默认）\n", model)
				logger.Info("[cron] 编译目标已解析", "compile_model", model)
			}
			compiler := cron.NewLLMCompiler(resolver)
			scriptExec := cron.NewScriptExecutor()
			scheduler = cron.NewScheduler(store.DB(), compiler, scriptExec)
			if err := scheduler.Init(ctx); err != nil {
				scheduler = nil
				fmt.Printf("  ✗ Cron        Init 失败 (%v)\n", err)
			} else {
				cronOK = true
				// Register the cron_task skill so the Agent can create/manage
				// built-in scheduled jobs from a conversation instead of
				// degrading to "write a script file + manual crontab".
				localAPIBase := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port)
				if err := skills.Register(builtin.NewCronTaskSkill(scheduler, localAPIBase)); err != nil {
					logger.Warn("[cron] cron_task skill registration failed", "err", err.Error())
				}
				// Agent-mode executor: cognitive jobs run one full Agent round
				// per tick. Wired BEFORE scheduler.Start (the start itself is
				// deferred until the desktop notifier is set, review L7).
				scheduler.SetAgentRunner(func(runCtx context.Context, job *cron.Job) (cron.AgentResult, error) {
					// NewCronDispatchMessage stamps source=cron, which the engine
					// relies on to skip the skill fast path and intent guidance.
					reply, err := eng.Process(runCtx, engine.NewCronDispatchMessage(
						job.UserID, job.ChatID, job.ID, job.SourcePrompt))
					if err != nil {
						return cron.AgentResult{}, err
					}
					// Pass the invoked tool names through so the scheduler can
					// verify a self-reported success against what the agent
					// actually did (e.g. an ingest job must call knowledge_ingest).
					names := make([]string, 0, len(reply.ToolCalls))
					for _, tc := range reply.ToolCalls {
						names = append(names, tc.Name)
					}
					return cron.AgentResult{Content: reply.Content, ToolNames: names}, nil
				})
				// In-process knowledge ingest for Starlark cron scripts. Loopback
				// SSRF blocking forbids a script from POSTing to the app's own KB
				// API, so the kb_ingest builtin writes straight through the manager.
				// Sourced from the engine because kbMgr is scoped to the
				// knowledge-init block above.
				if kb := eng.KnowledgeBase(); kb != nil {
					scheduler.SetKBIngest(func(ingestCtx context.Context, title, content, source string) (string, error) {
						doc, err := kb.AddDocument(ingestCtx, title, content, source)
						if err != nil {
							return "", err
						}
						return doc.ID, nil
					})
				}
			}
		}
	}
	if cronOK {
		fmt.Println("  ✓ Cron        调度器已启动 (v2 脚本编译模式)")
	} else if !cfg.Cron.Enabled {
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
				// §13.3(1) 事件触发：webhook 绑定了 job → 触发该 cron job 而非跑 agent
				// prompt。TriggerJob 是 fire-and-forget，executeJob 自建 runBudget ctx
				// （非 webhook 的 5min ctx），长任务不被砍（T1-4）。
				if event.JobID != "" {
					if scheduler == nil {
						return fmt.Errorf("webhook 绑定了 job %q 但调度器未就绪", event.JobID)
					}
					return scheduler.TriggerJob(ctx, event.JobID)
				}
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
		// D2.1 Layer 2 cron 自然语言解析 — 启动期取当前默认；后续会被 SetCronParserResolver 替换
		// （TODO 后续：parse 端点也走 resolver 路径，跟随 UI 切换）
		if resolver := buildCronProviderResolver(router); resolver != nil {
			if p, m, err := resolver(); err == nil && p != nil {
				srv.SetCronParser(p, m)
			}
		}
	}
	if fileMem != nil {
		srv.SetFileMemory(fileMem)
	}
	if vm := eng.GetVectorMemory(); vm != nil {
		srv.SetVectorMemory(vm)
	}

	// 挂载 Phase 4 API 端点
	if mcpMgr != nil {
		srv.SetMCPManager(mcpMgr)
	}
	// MCP 动态添加持久化 (P0 修复: HTTP API 添加的 MCP server 也要持久化)
	if home, err := os.UserHomeDir(); err == nil {
		srv.SetCfgWriter(config.NewWriter(filepath.Join(home, ".hexclaw", "hexclaw.yaml")))
	}
	if mp != nil {
		srv.SetMarketplace(mp)
	}

	// 12. 初始化多 Agent 路由 + 持久化
	agentRouter := agentrouter.New()
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(ctx); err != nil {
		logger.Error("Agent 存储初始化失败", "error", err)
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

	// 注册需要 dispatcher/executor 的 Agent 级 Skill
	if err := skills.Register(engine.NewHandoffSkill(agentRouter)); err != nil {
		logger.Error("注册 HandoffSkill 失败", "error", err)
	}
	// OrchestrateSkill + SpawnSkill 共享 executor: 通过 engine.Process 执行子任务
	agentExecFn := func(ctx context.Context, agentName, task string) (string, error) {
		msg := &adapter.Message{
			ID:       "sub-" + idgen.NanoID(),
			Platform: adapter.PlatformAPI,
			UserID:   "system",
			Content:  task,
			// "source":"spawn" lets the engine-side system-dispatch guard
			// recognize sub-task dispatches (sources: cron-dispatch /
			// heartbeat / webhook / spawn).
			Metadata: map[string]string{"role": agentName, "source": "spawn"},
		}
		reply, err := eng.Process(ctx, msg)
		if err != nil {
			return "", err
		}
		return reply.Content, nil
	}
	if err := skills.Register(engine.NewOrchestrateSkill(agentExecFn)); err != nil {
		logger.Error("注册 OrchestrateSkill 失败", "error", err)
	}
	if err := skills.Register(engine.NewSpawnSkill(agentExecFn)); err != nil {
		logger.Error("注册 SpawnSkill 失败", "error", err)
	}

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
			// Edge TTS 免费、无需 API Key，优先识别
			// （原代码只初始化 OpenAI TTS，edge-tts Provider 名被静默忽略 —— H5 修复）
			if cfg.Voice.TTS.Provider == "edge-tts" || cfg.Voice.TTS.Provider == "edge" {
				var edgeOpts []voice.EdgeTTSOption
				if cfg.Voice.TTS.Voice != "" {
					edgeOpts = append(edgeOpts, voice.EdgeTTSWithVoice(cfg.Voice.TTS.Voice))
				}
				tts = voice.NewEdgeTTS(edgeOpts...)
			} else {
				llmName := extractLLMName(cfg.Voice.TTS.Provider)
				if pc, ok := cfg.LLM.Providers[llmName]; ok && pc.APIKey != "" {
					var ttsOpts []voice.TTSOption
					if pc.BaseURL != "" {
						ttsOpts = append(ttsOpts, voice.TTSWithBaseURL(pc.BaseURL))
					}
					tts = voice.NewOpenAITTS(pc.APIKey, "", ttsOpts...)
				}
			}
		}

		// v0.4.0 F7 接入：当 cfg.Voice.TTS.Provider == "minimax,edge" 这类 chain 形式
		// 时（用 ',' 分隔的 provider 列表），自动构造 ChainedTTS。flag voice.tts.chain.v1
		// OFF 时只调第一个 provider。
		if strings.Contains(cfg.Voice.TTS.Provider, ",") {
			var multiCfg []voice.MultiTTSConfig
			for _, name := range strings.Split(cfg.Voice.TTS.Provider, ",") {
				name = strings.TrimSpace(name)
				switch name {
				case "minimax":
					if pc, ok := cfg.LLM.Providers["minimax"]; ok {
						multiCfg = append(multiCfg, voice.MultiTTSConfig{Provider: "minimax", APIKey: pc.APIKey, BaseURL: pc.BaseURL, GroupID: cfg.Voice.TTS.Region})
					}
				case "edge", "edge-tts":
					multiCfg = append(multiCfg, voice.MultiTTSConfig{Provider: "edge-tts"})
				}
			}
			if chained := voice.BuildMultiTTS(multiCfg); chained != nil {
				tts = chained
			}
		}

		voiceSvc = voice.NewService(stt, tts)
		srv.SetVoice(voiceSvc)
		fmt.Printf("  ✓ Voice       STT=%s, TTS=%s\n", voiceSvc.STTName(), voiceSvc.TTSName())
	} else {
		fmt.Println("  ✗ Voice       未启用")
	}

	// 14.4 生成内容存储（图像/视频持久化）
	// 与 SQLite 同目录，便于备份；位置 ~/.hexclaw/generated/
	// 提升到块外，使 media_generate Skill 注册可复用同一 store（落盘拿稳定路径）。
	var genStoreSvc *genstore.Store
	{
		home, _ := os.UserHomeDir()
		genStoreRoot := filepath.Join(home, ".hexclaw", "generated")
		if gs, gErr := genstore.NewStore(genStoreRoot); gErr == nil {
			genStoreSvc = gs
			srv.SetGenStore(gs)
			fmt.Printf("  ✓ GenStore    %s\n", genStoreRoot)
		} else {
			fmt.Printf("  ✗ GenStore    初始化失败: %v\n", gErr)
		}
	}

	// 14.5 初始化/热更新 image/video/voice chat 生成服务
	//
	// 从已配置的 LLM Provider 派生：任何带 API Key 的 provider 都可以参与生成能力。
	// 用闭包封装以便 LLM 配置热更新后重建（用户通过 UI 补 API Key → 这里重新拉 cfg）。
	// Provider map 的 key 是用户起的名字（可能是中文"智谱 AI"），不是 provider type，
	// 硬编码 cfg.LLM.Providers["zhipu"] 永远查不到。按 BaseURL 特征识别：
	findProviderByBaseURL := func(substrs ...string) (apiKey, baseURL string) {
		for _, p := range cfg.LLM.Providers {
			if p.APIKey == "" {
				continue
			}
			lowered := strings.ToLower(p.BaseURL)
			for _, s := range substrs {
				if strings.Contains(lowered, s) {
					return p.APIKey, p.BaseURL
				}
			}
		}
		return "", ""
	}
	reloadGenServices := func(verbose bool) (ig *imagegen.Service, vg *videogen.Service, vc *voicechat.Service) {
		// Image gen: OpenAI DALL-E / 智谱 CogView
		ip := map[string]imagegen.Provider{}
		if k, b := findProviderByBaseURL("openai.com", "/v1"); k != "" && strings.Contains(strings.ToLower(b), "openai") {
			ip["openai-dalle"] = imagegen.NewOpenAIDallE(k, b)
		}
		if k, b := findProviderByBaseURL("bigmodel.cn"); k != "" {
			ip["zhipu-cogview"] = imagegen.NewZhipuCogView(k, b)
		}
		igDefault := ""
		if _, ok := ip["openai-dalle"]; ok {
			igDefault = "openai-dalle"
		} else if _, ok := ip["zhipu-cogview"]; ok {
			igDefault = "zhipu-cogview"
		}
		ig = imagegen.NewService(ip, igDefault)
		srv.SetImageGen(ig)

		// Video gen: 智谱 CogVideoX
		vp := map[string]videogen.Provider{}
		if k, b := findProviderByBaseURL("bigmodel.cn"); k != "" {
			vp["zhipu-cogvideox"] = videogen.NewZhipuCogVideoX(k, b)
		}
		vgDefault := ""
		if _, ok := vp["zhipu-cogvideox"]; ok {
			vgDefault = "zhipu-cogvideox"
		}
		vg = videogen.NewService(vp, vgDefault)
		srv.SetVideoGen(vg)

		// 引擎内联媒体生成复用上面构造的 image/video Service（均已是 ai-core/media，
		// 路线图 §12 risk#5：媒体作为独立内聚包，不再经 LLM Provider 能力接口）。
		eng.SetMediaServices(ig, vg)

		// Voice chat: OpenAI gpt-4o-audio
		cp := map[string]voicechat.Provider{}
		if k, b := findProviderByBaseURL("openai.com"); k != "" {
			cp["openai-gpt4o-audio"] = voicechat.NewOpenAIVoiceChat(k, b)
		}
		vcDefault := ""
		if _, ok := cp["openai-gpt4o-audio"]; ok {
			vcDefault = "openai-gpt4o-audio"
		}
		vc = voicechat.NewService(cp, vcDefault)
		srv.SetVoiceChat(vc)

		if verbose {
			if ig.HasProvider() {
				fmt.Printf("  ✓ ImageGen    %d providers (%v)\n", len(ip), ig.Providers())
			} else {
				fmt.Println("  ✗ ImageGen    无可用 Provider（需要配置 OpenAI 或智谱 API Key）")
			}
			if vg.HasProvider() {
				fmt.Printf("  ✓ VideoGen    %d providers (%v)\n", len(vp), vg.Providers())
			} else {
				fmt.Println("  ✗ VideoGen    无可用 Provider（需要配置智谱 API Key）")
			}
			if vc.HasProvider() {
				fmt.Printf("  ✓ VoiceChat   %d providers (%v)\n", len(cp), vc.Providers())
			} else {
				fmt.Println("  ✗ VoiceChat   无可用 Provider（需要 OpenAI API Key + gpt-4o-audio-preview）")
			}
		}
		return
	}
	imagegenSvc, videogenSvc, voiceChatSvc := reloadGenServices(true)
	// 配置热更新时重建 gen services（读最新 cfg.LLM.Providers）
	srv.SetGenServicesReloader(func() { reloadGenServices(false) })

	// 媒体生成 Skill：默认关 + 有 Provider 才注册。
	// 直调 Generate/Submit + blobstore 落盘，返回稳定 FilePath 供 export/send/ingest 串联。
	if cfg.Skill.Builtin.MediaGen && imagegenSvc != nil && imagegenSvc.HasProvider() {
		if err := skills.Register(builtin.NewMediaGenerateSkill(imagegenSvc, videogenSvc, genStoreSvc)); err != nil {
			logger.Warn("[media] failed to register media_generate skill", "err", err.Error())
		} else {
			fmt.Println("  ✓ Skill       media_generate (§11.1)")
		}
	}

	// 15. 初始化桌面集成服务（Phase 6）
	desktopSvc := desktop.NewService(version)
	srv.SetDesktop(desktopSvc)
	if scheduler != nil {
		// Push heal results and agent job deliveries to the desktop
		// notification center, mapping the scheduler's level onto the desktop
		// notification type (success heals are not warnings, review L6).
		scheduler.SetNotifier(func(_ *cron.Job, level, title, body string) {
			nt := desktop.NotifyWarning
			switch level {
			case cron.NotifyLevelInfo:
				nt = desktop.NotifyInfo
			case cron.NotifyLevelSuccess:
				nt = desktop.NotifySuccess
			}
			desktopSvc.Notify(title, body, nt)
		})
		// Start only after the agent runner AND the notifier are wired
		// (review L7): jobs due right at boot would otherwise run before
		// delivery / heal notifications were possible.
		scheduler.Start(ctx)
	}

	// 16. 文档渲染服务（POST /api/v1/render）
	//
	// markdown → docx/pdf/epub/odt/rtf/txt/html/md 通过 pandoc 渲染。
	// 详见 .claude/doc-generation-architecture.md
	if renderSvc, err := buildRenderService(); err == nil && renderSvc != nil {
		srv.SetRenderService(renderSvc)
		logger.Info("[info] 文档渲染服务已启用",
			"engine", "pandoc",
			"endpoint", "/api/v1/render")
		// 文档导出 Skill：默认关 + render service 就绪才注册。
		// 包一层 render.Service，返回落盘路径供送达附件 / 入库 / 下载串联。
		if cfg.Skill.Builtin.ExportDoc {
			if rerr := skills.Register(builtin.NewExportDocumentSkill(renderSvc)); rerr != nil {
				logger.Warn("[render] failed to register export_document skill", "err", rerr.Error())
			} else {
				fmt.Println("  ✓ Skill       export_document (§11.3)")
			}
		}
	} else if err != nil {
		logger.Warn("[warn] 文档渲染服务未启用", "reason", err.Error())
	} else {
		logger.Info("[info] 文档渲染服务未启用",
			"reason", "系统未安装 pandoc，POST /api/v1/render 端点不挂载")
	}

	// 抑制未使用变量警告
	_ = agentRouter
	_ = canvasSvc
	_ = voiceSvc
	_ = imagegenSvc
	_ = videogenSvc
	_ = voiceChatSvc

	messageHandler := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		if err := gw.Check(ctx, msg); err != nil {
			return &adapter.Reply{Content: "安全检查未通过: " + err.Error()}, nil
		}
		return eng.Process(ctx, msg)
	}

	instanceMgr := instances.NewManager(store.DB())
	// 静态加密：主密钥与 SQLite 同目录（~/.hexclaw/master.key, 0600）。加载失败不阻断启动，
	// 降级为明文直存（保持可用），仅告警；绝不记录密钥或明文凭据。
	dataDir := filepath.Dir(cfg.Storage.SQLite.Path)
	var secretBox *secret.Box
	if box, berr := secret.LoadBox(dataDir); berr != nil {
		logger.Warn("[secret] 加载主密钥失败，凭据将以明文存储", "err", berr.Error())
	} else {
		secretBox = box
		instanceMgr.SetSecretBox(box)
	}
	if err := instanceMgr.Init(ctx); err != nil {
		return fmt.Errorf("初始化平台实例运行时失败: %w", err)
	}
	if err := instanceMgr.SeedFromConfig(ctx, cfg); err != nil {
		return fmt.Errorf("写入平台实例种子失败: %w", err)
	}
	// 历史明文 config_json 静态加密回填（box 未注入时为 no-op）。
	if n, eerr := instanceMgr.EncryptExistingAtRest(ctx); eerr != nil {
		logger.Warn("[secret] 历史凭据静态加密回填失败", "err", eerr.Error())
	} else if n > 0 {
		logger.Info("[secret] 历史凭据已静态加密", "count", n)
	}
	instanceMgr.SetHandler(messageHandler)
	srv.SetInstanceManager(instanceMgr)

	// §15.1 数据连接器：token 只读接入 GitHub / Notion（token 复用同一 secret.Box 加密落盘）。
	srv.SetConnectorStore(connector.NewStore(dataDir, secretBox))

	// 多通道送达 Skill：复用同源 live adapters（内部经 per-platform SendQueue 限速），
	// 不自己 reach into adapters。§11.10 统一安全闸：发送审批由 engine PermissionPolicy
	// 的 send-approve 规则 + 无人值守风险顾问统一执行，Skill 是纯发送器、不自管确认门。
	if cfg.Skill.Builtin.SendMessage {
		sender := &instanceMessageSender{mgr: instanceMgr, ctx: ctx}
		if err := skills.Register(builtin.NewSendMessageSkill(sender)); err != nil {
			logger.Warn("[send] failed to register send_message skill", "err", err.Error())
		} else {
			fmt.Println("  ✓ Skill       send_message (§11.2 + §11.10 unified gate)")
		}
	}

	if scheduler != nil {
		// Route cron jobs' IM deliver targets (feishu/discord/...) through the
		// running platform adapters. Desktop-class targets still go via the
		// notifier wired above; this seam makes IM delivery actually send rather
		// than only log (review L2).
		scheduler.SetDeliverer(func(job *cron.Job, target, content string) error {
			if job.ChatID == "" {
				return fmt.Errorf("job %s has no chat_id for IM target %q", job.ID, target)
			}
			return instanceMgr.Send(ctx, target, job.ChatID, &adapter.Reply{Content: content})
		})
	}

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
			srv.SetStreamStateProvider(wa)

			// 接通工具审批: WebAdapter ↔ PermissionHub
			wa.SetApprovalResponseHandler(func(reqID string, approved, remember bool) {
				permHub.HandleResponse(engine.PermissionResponse{
					RequestID: reqID,
					Approved:  approved,
					Remember:  remember,
				})
			})
			permHub.SetSender(&webPermissionBridge{wa: wa})
			fmt.Println("  ✓ Permission  WebSocket 审批已接通")

			// 接通记忆提取通知: Engine → WebAdapter → 前端
			eng.SetOnMemorySaved(func(content string) {
				wa.Broadcast("memory_saved", content, nil)
			})
		}
	}
	if err := instanceMgr.StartEnabled(ctx); err != nil {
		return fmt.Errorf("启动平台实例失败: %w", err)
	}

	// 监听退出信号，优雅关闭
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	readyCh := make(chan struct{})
	onReady := func() {
		// 端口真实 bind 成功后才打印"已就绪"相关信息，历史 bug：先打印再 bind，
		// 启动卡住时用户看日志以为已启动，实际 HTTP 从未监听。
		lc.Info("system", fmt.Sprintf("Web UI: http://%s:%d | Chat API: POST /api/v1/chat", cfg.Server.Host, cfg.Server.Port))
		lc.Info("system", "🦀 HexClaw 已就绪 — 数据全在本地，横行无忧")
		close(readyCh)
	}
	go func() {
		if err := srv.Start(sigCtx, onReady); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// 启动 watchdog：30 秒内若 HTTP 未 bind 成功则主动退出，避免"进程在跑但端口死"的无响应状态
	go func() {
		const startupDeadline = 30 * time.Second
		select {
		case <-readyCh:
			return
		case <-time.After(startupDeadline):
			logger.Error("启动超时：HTTP 服务未在 30 秒内 bind 端口，主动退出以便上层重启或诊断",
				"host", cfg.Server.Host, "port", cfg.Server.Port)
			os.Exit(1)
		case <-sigCtx.Done():
			return
		}
	}()

	// v0.4.x C4 组合学习 ticker：每 30 分钟扫一次最近窗口，把高频成功序列写成 meta-Skill 草稿。
	// shutdown 时随 sigCtx 退出，不阻塞 graceful shutdown。
	go func() {
		const interval = 30 * time.Minute
		// 启动后等 5 分钟再开第一轮，避免冷启窗口为空时空跑
		warmup := time.NewTimer(5 * time.Minute)
		defer warmup.Stop()
		select {
		case <-sigCtx.Done():
			return
		case <-warmup.C:
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		runOnce := func() {
			cands := improveStore.SuggestMetaSkills()
			for _, c := range cands {
				if err := improveStore.WriteMetaDraft(c); err != nil {
					logger.Warn("写 meta-skill 草稿失败", "steps", c.Steps, "error", err)
				} else {
					logger.Info("写出 meta-skill 草稿", "steps", c.Steps,
						"occur", c.OccurCount, "success_rate", c.SuccessRate)
				}
			}
		}
		runOnce()
		for {
			select {
			case <-sigCtx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
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
		logger.Error("error", "error", err)
	}

	if err := instanceMgr.StopAll(shutdownCtx); err != nil {
		logger.Error("error", "error", err)
	}

	fmt.Println("  🦀 HexClaw 已停止")
	return nil
}

// webPermissionBridge adapts WebAdapter to engine.PermissionSender interface.
// Breaks circular dependency: engine → adapter/web.
type webPermissionBridge struct {
	wa *webadapter.WebAdapter
}

func (b *webPermissionBridge) SendPermissionRequest(ctx context.Context, sessionID string, req *engine.PermissionRequest) error {
	return b.wa.SendPermissionRequest(ctx, sessionID, &webadapter.PermissionRequestData{
		ID:        req.ID,
		ToolName:  req.ToolName,
		Arguments: req.Arguments,
		Risk:      req.Risk,
		Reason:    req.Reason,
	})
}

// instanceMessageSender 把 send_message Skill 的 MessageSender 接到 live
// 平台适配器：channel = provider/instance（feishu/discord/...），target = chatID。
// 经 instanceMgr.Send → adapter.Send，内部走 per-platform SendQueue 限速（与 cron Deliverer 同源）。
//
// TODO: atts（附件）暂未透传 —— adapter.Reply 当前 Content-only；导出文档作附件送达需先把
// RenderResult 落盘路径包成 adapter.Attachment 并扩展 Reply，留到下游串联时接。
// unattendedRiskAdapter 把 builtin.RiskReviewer（判级 low/med/high）适配成 engine
// 的无人值守顾问：仅 low 且无错放行，其余 fail-closed。§11.10 统一安全闸的判级源。
type unattendedRiskAdapter struct{ r builtin.RiskReviewer }

func (a unattendedRiskAdapter) AssessLowRisk(ctx context.Context, action, payload string) bool {
	lvl, err := a.r.Assess(ctx, action, payload)
	return err == nil && lvl == builtin.RiskLow
}

type instanceMessageSender struct {
	mgr *instances.Manager
	ctx context.Context
}

func (s *instanceMessageSender) Send(ctx context.Context, channel, target, content string, _ []adapter.Attachment) error {
	if ctx == nil {
		ctx = s.ctx
	}
	return s.mgr.Send(ctx, channel, target, &adapter.Reply{Content: content})
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

// countEnabledFlags 统计当前 Flags 实例中"启用"的 flag 数量。仅启动日志展示用。
func countEnabledFlags(flags featureflag.Flags) int {
	count := 0
	for _, s := range flags.Snapshot() {
		if s.Enabled {
			count++
		}
	}
	return count
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

// pickCronCompilerProvider 为 cron 编译挑一个可用 LLM provider。
//
// Cron 编译需要稳定快速的远端模型（典型 prompt ~600 tokens，期望 < 15s）。
// 本地 Ollama 在 9B-级模型上 p50 ~ 30-60s + 拖慢用户机器，不适合做 cron 编译。
//
// 优先级：
//  1. 远端 provider（含 api_key，且名字含 openrouter / anthropic / openai /
//     claude / gpt / deepseek / 智谱 等关键词）
//  2. 任意非 Ollama 的 provider（可能用户私有 gateway 命名规律不可知）
//  3. Ollama 兜底（仅作为最后的选项，避免完全跑不了）
//
// 显式忽略 router.Default()，因为它可能被配置漂移指向不存在的 key
// 或 fallback 到 Ollama，掩盖更快的远端 provider。
// cronRouterView 是 buildCronProviderResolver 选型逻辑所需的 router 最小只读视图。
// *llmrouter.Selector 天然满足它；抽成接口纯为单测可注入 fake（见 cron_compile_target_test.go）。
type cronRouterView interface {
	DefaultName() string
	Providers() []string
	ProviderConfig(name string) (config.LLMProviderConfig, bool)
	Get(name string) (hexagon.Provider, bool)
}

// buildCronProviderResolver 构造一个 closure — 每次 Compile 时调用，
// 解析 cron 脚本编译用的 provider + model（动态跟随用户 UI 切换，无需重启）。
//
// 选型优先级（用户决策 2026-05-29：cron 编译优先走「可用的远程快模型」，
// 对话仍用用户选中的默认 —— 显式的子系统分流，不是偷偷兜底）：
//  1. 默认 provider 本身就是「远程 + chat」→ 直接用默认（尊重用户显式选择）。
//  2. 默认是本地（如 Ollama，9B 编译 cron 实测 >300s）→ 挑一个「远程 + chat」
//     provider 跑编译（装机环境自动命中智谱 glm-5.1）。
//  3. 没有任何远程可用 → 退回默认（本地），保留非 chat 模型守卫；模型太慢时
//     由 llmcall/SSE 链路返回 humanize 后的友好错误（不会静默卡死）。
//
// 全程不静默换成「错误类别」的模型：非 chat（图像/嵌入/语音）一律跳过并最终友好报错。
func buildCronProviderResolver(r *llmrouter.Selector) cron.ProviderResolver {
	return func() (hexagon.Provider, string, error) {
		if r == nil {
			return nil, "", fmt.Errorf("LLM router 未初始化")
		}
		return pickCronCompileTarget(r)
	}
}

// pickCronCompileTarget 按「远程 chat 优先、本地兜底」优先级选 cron 编译目标。
func pickCronCompileTarget(rv cronRouterView) (hexagon.Provider, string, error) {
	defName := rv.DefaultName()
	if defName == "" {
		return nil, "", fmt.Errorf("未配置默认 LLM provider — 请到设置 → LLM 选一个 chat 模型作为默认")
	}
	// 优先尝试远程 chat 候选（默认若为远程则排最前，其余远程按名字稳定排序）。
	for _, name := range cronCompileCandidates(rv, defName) {
		if p, model, err := resolveChatProvider(rv, name); err == nil {
			return p, model, nil
		}
	}
	// 没有远程 chat 可用 → 退回默认（本地）；返回其详细错误语义（成功则走本地慢路径）。
	return resolveChatProvider(rv, defName)
}

// cronCompileCandidates 返回远程 chat 候选的有序列表：
// 默认 provider 若为远程排最前（尊重用户显式选择），其余远程 provider 按名字排序。
// 默认为本地时不在此列出（由 pickCronCompileTarget 末尾兜底）。
func cronCompileCandidates(rv cronRouterView, defName string) []string {
	var remotes []string
	for _, name := range rv.Providers() {
		if name == defName || !isRemoteProvider(rv, name) {
			continue
		}
		remotes = append(remotes, name)
	}
	sort.Strings(remotes)
	out := make([]string, 0, len(remotes)+1)
	if isRemoteProvider(rv, defName) {
		out = append(out, defName)
	}
	return append(out, remotes...)
}

// isRemoteProvider 判定 provider 是否为远程（非 localhost / 127.0.0.1）。
func isRemoteProvider(rv cronRouterView, name string) bool {
	pc, ok := rv.ProviderConfig(name)
	if !ok {
		return false
	}
	return !strings.Contains(pc.BaseURL, "localhost") && !strings.Contains(pc.BaseURL, "127.0.0.1")
}

// resolveChatProvider 校验单个 provider 可用且为 chat completion 类，返回 provider+model 或详细错误。
func resolveChatProvider(rv cronRouterView, name string) (hexagon.Provider, string, error) {
	p, ok := rv.Get(name)
	if !ok || p == nil {
		return nil, "", fmt.Errorf("LLM provider (%s) 不可用 — 检查 API key / 网络", name)
	}
	pc, ok := rv.ProviderConfig(name)
	if !ok {
		return nil, "", fmt.Errorf("LLM provider (%s) 配置缺失", name)
	}
	model := pc.Model
	if model == "" {
		return nil, "", fmt.Errorf("LLM provider (%s) 未指定 model", name)
	}
	if isNonChatModel(model) {
		return nil, "", fmt.Errorf("LLM (%s / %s) 不是 chat completion 模型（疑似图像/嵌入/语音类）— 请到设置 → LLM 改选 chat 类模型", name, model)
	}
	return p, model, nil
}

// isNonChatModel 识别图像/嵌入/语音等非 chat completion 类模型名。
// 装机时智谱默认 cogview-4 是图像模型，喂给 LLMCompiler 会返 content=array 触发 json 解析失败。
func isNonChatModel(m string) bool {
	n := strings.ToLower(m)
	for _, kw := range []string{
		"cogview", "dall", "midjourney", "stable-diffusion", "sd-",
		"embedding", "embed-", "bge-", "m3e", "text-embedding", "nomic-embed",
		"whisper", "tts", "audio-", "voice-",
		"cogvideo", "video-",
	} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// pickFastCompileModel 已删除（2026-05-27 用户反馈）：
// 不再偷换模型 — cron compiler 直接用用户在设置中选中的默认 provider.Model。

// buildRenderService 装配文档渲染服务。
//
// 资产路径：
//   - sandbox: ~/.hexclaw/render/sandbox（temp file 存放）
//   - cache:   ~/.hexclaw/render/cache  （sha256 → file 命中复用）
//
// 仅当系统 PATH 上有 pandoc 时才返回非空 Service；否则返回 nil（端点不启用，
// 走 fallback 错误码 ENGINE_MISSING）。typst 缺失时仍可启用（PDF 单独失败）。
//
// engine_version 字符串由 pandoc / typst 二进制版本拼接，参与缓存键计算；
// 升级二进制后字符串变化 → 旧缓存自动失效。
func buildRenderService() (*render.Service, error) {
	// 必须有 pandoc 才启用
	if _, err := exec.LookPath("pandoc"); err != nil {
		return nil, nil //nolint:nilnil // 不报错，只是不启用
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}
	sandbox := filepath.Join(home, ".hexclaw", "render", "sandbox")
	cacheDir := filepath.Join(home, ".hexclaw", "render", "cache")

	renderer, err := render.NewPandocRenderer("", "", sandbox)
	if err != nil {
		return nil, err
	}

	// engine_version 拼接 pandoc + typst 版本
	engineVer := pandocVersion()
	if tv := typstVersion(); tv != "" {
		engineVer += "+" + tv
	}

	// 计算 renderer_assets_hash：P0 内置资产清单（任一资产改动让缓存失效）；
	// 同时把第一份资产（默认 reference.docx）注入 PandocRenderer，让 pandoc 实际消费它。
	assetPaths := resolveRenderAssetPaths()
	assetsHash, err := render.AssetsHash(assetPaths)
	if err != nil {
		return nil, fmt.Errorf("compute assets hash: %w", err)
	}
	if len(assetPaths) > 0 {
		if _, statErr := os.Stat(assetPaths[0]); statErr == nil {
			renderer.ReferenceDocPath = assetPaths[0]
		}
	}

	cache, err := render.NewCache(render.CacheConfig{
		Dir:           cacheDir,
		MaxBytes:      render.CacheMaxBytes,
		TTL:           render.CacheTTL,
		EngineVersion: engineVer,
		AssetsHash:    assetsHash,
		DefaultLocale: "zh-CN",
	})
	if err != nil {
		return nil, err
	}

	return render.NewService(render.ServiceConfig{Renderer: renderer, Cache: cache})
}

// resolveRenderAssetPaths 返回 renderer_assets_hash 计算用的 P0 资产清单。
//
// 当前内置资产：
//   - reference.docx：默认 docx 字体/页边距/标题样式
//
// 资产文件不存在时 AssetsHash 视为零字节（不阻塞启动）；release 真正捆绑后
// 此清单自动生效，hash 变化让缓存失效。
func resolveRenderAssetPaths() []string {
	candidates := []string{}
	// 桌面 .app 捆绑：HEXCLAW_RESOURCE_DIR 由 Tauri sidecar.rs 注入，指向 Contents/Resources/
	if dir := os.Getenv("HEXCLAW_RESOURCE_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "assets", "render", "reference.docx"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "assets", "render", "reference.docx"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "render", "assets", "reference.docx"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".hexclaw", "render", "assets", "reference.docx"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return []string{p}
		}
	}
	if len(candidates) > 0 {
		return []string{candidates[0]}
	}
	return nil
}

// pandocVersion 取 pandoc 版本号（用于 engine_version 缓存键维度）。
// 失败返回 "pandoc-unknown"，不阻塞启动。
func pandocVersion() string {
	out, err := exec.Command("pandoc", "--version").Output()
	if err != nil {
		return "pandoc-unknown"
	}
	// 第一行形如 "pandoc 3.9.0.2"
	first := strings.SplitN(string(out), "\n", 2)[0]
	parts := strings.Fields(first)
	if len(parts) >= 2 {
		return "pandoc-" + parts[1]
	}
	return "pandoc-unknown"
}

// typstVersion 取 typst 版本号；缺失返回空串。
func typstVersion() string {
	if _, err := exec.LookPath("typst"); err != nil {
		return ""
	}
	out, err := exec.Command("typst", "--version").Output()
	if err != nil {
		return "typst-unknown"
	}
	parts := strings.Fields(string(out))
	if len(parts) >= 2 {
		return "typst-" + parts[1]
	}
	return "typst-unknown"
}
