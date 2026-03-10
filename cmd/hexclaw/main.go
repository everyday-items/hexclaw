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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/adapter/dingtalk"
	"github.com/everyday-items/hexclaw/internal/adapter/discord"
	"github.com/everyday-items/hexclaw/internal/adapter/feishu"
	"github.com/everyday-items/hexclaw/internal/adapter/slack"
	"github.com/everyday-items/hexclaw/internal/adapter/telegram"
	webadapter "github.com/everyday-items/hexclaw/internal/adapter/web"
	"github.com/everyday-items/hexclaw/internal/adapter/wechat"
	"github.com/everyday-items/hexclaw/internal/adapter/wecom"
	"github.com/everyday-items/hexclaw/internal/api"
	"github.com/everyday-items/hexclaw/internal/audit"
	"github.com/everyday-items/hexclaw/internal/canvas"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/desktop"
	"github.com/everyday-items/hexclaw/internal/cron"
	"github.com/everyday-items/hexclaw/internal/engine"
	"github.com/everyday-items/hexclaw/internal/gateway"
	"github.com/everyday-items/hexclaw/internal/heartbeat"
	"github.com/everyday-items/hexclaw/internal/knowledge"
	"github.com/everyday-items/hexclaw/internal/llmrouter"
	hexmcp "github.com/everyday-items/hexclaw/internal/mcp"
	"github.com/everyday-items/hexclaw/internal/memory"
	agentrouter "github.com/everyday-items/hexclaw/internal/router"
	"github.com/everyday-items/hexclaw/internal/skill"
	"github.com/everyday-items/hexclaw/internal/skill/builtin"
	"github.com/everyday-items/hexclaw/internal/skill/marketplace"
	sqlitestore "github.com/everyday-items/hexclaw/internal/storage/sqlite"
	"github.com/everyday-items/hexclaw/internal/voice"
	"github.com/everyday-items/hexclaw/internal/webhook"
)

// 版本信息，通过 -ldflags 注入
var (
	version = "dev"
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
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 HexClaw 服务",
		Long: `启动 HexClaw 服务，包含 Agent 引擎、安全网关、平台适配器、REST API。

示例:
  hexclaw serve
  hexclaw serve --config hexclaw.yaml
  hexclaw serve --feishu-app-id xxx --feishu-app-secret xxx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(configFile, feishuAppID, feishuSecret, telegramToken)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "", "配置文件路径 (默认 ~/.hexclaw/hexclaw.yaml)")
	cmd.Flags().StringVar(&feishuAppID, "feishu-app-id", "", "飞书 App ID")
	cmd.Flags().StringVar(&feishuSecret, "feishu-app-secret", "", "飞书 App Secret")
	cmd.Flags().StringVar(&telegramToken, "telegram-token", "", "Telegram Bot Token")

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
func runServe(configFile, feishuAppID, feishuSecret, telegramToken string) error {
	// 1. 加载配置
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 命令行参数覆盖配置文件
	if feishuAppID != "" {
		cfg.Platforms.Feishu.Enabled = true
		cfg.Platforms.Feishu.AppID = feishuAppID
	}
	if feishuSecret != "" {
		cfg.Platforms.Feishu.AppSecret = feishuSecret
	}
	if telegramToken != "" {
		cfg.Platforms.Telegram.Enabled = true
		cfg.Platforms.Telegram.Token = telegramToken
	}

	fmt.Printf("HexClaw %s 启动中...\n", version)
	fmt.Printf("  监听地址: %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("  默认 LLM: %s\n", cfg.LLM.Default)

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
	log.Println("存储已初始化: SQLite")

	// 3. 初始化 LLM 路由
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		return fmt.Errorf("初始化 LLM 路由失败: %w", err)
	}
	fmt.Printf("  LLM Providers: %v\n", router.Providers())

	// 4. 初始化 Skill 注册中心
	skills := skill.NewRegistry()
	builtin.RegisterAll(skills, cfg.Skill.Builtin)

	// 4.5 加载 Markdown 技能（技能市场）
	var mp *marketplace.Marketplace
	if cfg.Skills.Enabled {
		mp = marketplace.NewMarketplace(cfg.Skills.Dir)
		if err := mp.Init(); err != nil {
			log.Printf("技能市场初始化失败: %v", err)
		} else {
			// 将 Markdown 技能注册到 Skill 注册中心
			mdSkills := mp.List()
			registered := 0
			for _, mdSkill := range mdSkills {
				// 包装为 skill.Skill 接口
				wrapper := &markdownSkillWrapper{skill: mdSkill}
				if err := skills.Register(wrapper); err != nil {
					log.Printf("注册 Markdown 技能 %q 失败: %v", mdSkill.Meta.Name, err)
				} else {
					registered++
				}
			}
			if registered > 0 {
				fmt.Printf("  Markdown 技能: %d 个\n", registered)
			}
		}
	}

	// 4.6 连接 MCP Server
	var mcpMgr *hexmcp.Manager
	if cfg.MCP.Enabled && len(cfg.MCP.Servers) > 0 {
		mcpMgr = hexmcp.NewManager()
		// 转换配置格式
		var mcpConfigs []hexmcp.ServerConfig
		for _, s := range cfg.MCP.Servers {
			enabled := s.Enabled
			if !enabled && s.Command != "" || s.Endpoint != "" {
				enabled = true // 兼容未显式设置 enabled 的配置
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
			log.Printf("MCP 连接出错: %v", err)
		}
		if totalTools > 0 {
			fmt.Printf("  MCP 工具: %d 个 (来自 %d 个 Server)\n", totalTools, len(mcpMgr.ServerNames()))
		}
		defer mcpMgr.Close()
	}

	// 5. 初始化安全网关
	gw := gateway.NewPipeline(&cfg.Security, store)

	// 6. 创建并启动 Agent 引擎
	eng := engine.NewReActEngine(cfg, router, store, skills)

	// 6.5 初始化知识库（向量搜索 + FTS5 混合检索）
	if cfg.Knowledge.Enabled {
		kbStore := knowledge.NewSQLiteStore(store.DB())
		if err := kbStore.Init(ctx); err != nil {
			log.Printf("知识库初始化失败: %v", err)
		} else {
			// 创建 Embedder（使用配置的 LLM Provider 生成向量）
			var embedder knowledge.Embedder
			if cfg.Knowledge.Embedding.Provider != "" {
				// TODO: 根据配置创建对应的 Embedder
				// 当前先使用 nil（退化为纯关键词搜索）
				log.Printf("Embedding Provider: %s (待集成)", cfg.Knowledge.Embedding.Provider)
			}

			// 混合检索配置
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

			// chunk 配置
			var opts []knowledge.ManagerOption
			if cfg.Knowledge.ChunkSize > 0 {
				opts = append(opts, knowledge.WithChunkSize(cfg.Knowledge.ChunkSize))
			}
			if cfg.Knowledge.ChunkOverlap > 0 {
				opts = append(opts, knowledge.WithChunkOverlap(cfg.Knowledge.ChunkOverlap))
			}
			opts = append(opts, knowledge.WithHybridConfig(hybridCfg))

			kbMgr := knowledge.NewManager(kbStore, embedder, opts...)
			eng.SetKnowledgeBase(kbMgr)
			log.Println("知识库已启用（FTS5 + 向量混合检索）")
		}
	}

	// 7. 初始化文件记忆系统
	var fileMem *memory.FileMemory
	if cfg.FileMemory.Enabled {
		var err error
		fileMem, err = memory.New(memory.Config{
			Enabled:   true,
			Dir:       cfg.FileMemory.Dir,
			MaxMemory: cfg.FileMemory.MaxMemory,
			DailyDays: cfg.FileMemory.DailyDays,
		})
		if err != nil {
			log.Printf("文件记忆初始化失败: %v", err)
		} else {
			// 加载记忆上下文注入引擎
			memCtx := fileMem.LoadContext()
			if memCtx != "" {
				log.Printf("文件记忆已加载 (%d 字符)", len(memCtx))
			}
		}
	}

	if err := eng.Start(ctx); err != nil {
		return fmt.Errorf("启动引擎失败: %w", err)
	}
	defer eng.Stop(context.Background())

	// 8. 启动 HTTP 服务
	srv := api.NewServer(cfg, eng, gw)

	// 挂载知识库 API
	if eng.KnowledgeBase() != nil {
		srv.SetKnowledgeBase(eng.KnowledgeBase())
	}

	// 9. 初始化定时任务调度器
	var scheduler *cron.Scheduler
	if cfg.Cron.Enabled {
		scheduler = cron.NewScheduler(store.DB())
		if err := scheduler.Init(ctx); err != nil {
			log.Printf("定时任务数据库初始化失败: %v", err)
			scheduler = nil
		} else {
			// 启动调度器，设置任务执行回调（通过引擎处理）
			scheduler.Start(ctx, func(ctx context.Context, job *cron.Job) error {
				_, err := eng.Process(ctx, &adapter.Message{
					Platform: adapter.PlatformAPI,
					UserID:   job.UserID,
					Content:  job.Prompt,
					Metadata: map[string]string{"source": "cron", "job_id": job.ID},
				})
				return err
			})
			defer scheduler.Stop()
			log.Println("定时任务调度器已启动")
		}
	}

	// 10. 初始化 Webhook 管理器
	var webhookMgr *webhook.Manager
	if cfg.Webhook.Enabled {
		webhookMgr = webhook.NewManager(store.DB())
		if err := webhookMgr.Init(ctx); err != nil {
			log.Printf("Webhook 初始化失败: %v", err)
			webhookMgr = nil
		} else {
			// 设置事件处理回调
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
			log.Println("Webhook 管理器已启用")
		}
	}

	// 11. 初始化心跳巡查
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

		hb := heartbeat.New(hbCfg)
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
			log.Printf("Heartbeat 通知: %s", message)
			// TODO: 通过已启用的适配器推送通知
			return nil
		}
		hb.Start(ctx, executor, notifier)
		defer hb.Stop()
		log.Printf("心跳巡查已启动（间隔 %d 分钟）", intervalMins)
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

	// 12. 初始化多 Agent 路由（Phase 5）
	var agentRouter *agentrouter.Router
	if cfg.Router.Enabled {
		agentRouter = agentrouter.New()
		srv.SetAgentRouter(agentRouter)
		log.Println("多 Agent 路由已启用")
	}

	// 13. 初始化 Canvas/A2UI 服务（Phase 5）
	var canvasSvc *canvas.Service
	if cfg.Canvas.Enabled {
		canvasSvc = canvas.NewService()
		srv.SetCanvas(canvasSvc)
		log.Println("Canvas/A2UI 已启用")
	}

	// 14. 初始化语音服务（Phase 5）
	var voiceSvc *voice.Service
	if cfg.Voice.Enabled {
		// TODO: 根据配置创建具体的 STT/TTS Provider
		voiceSvc = voice.NewService(nil, nil)
		srv.SetVoice(voiceSvc)
		log.Printf("语音服务已启用 (STT: %s, TTS: %s)", cfg.Voice.STT.Provider, cfg.Voice.TTS.Provider)
	}

	// 15. 初始化桌面集成服务（Phase 6）
	desktopSvc := desktop.NewService(version)
	srv.SetDesktop(desktopSvc)

	// 抑制未使用变量警告（后续模块集成时使用）
	_ = agentRouter
	_ = canvasSvc
	_ = voiceSvc

	// 8. 启动平台适配器
	var adapters []adapter.Adapter

	// 消息处理回调（适配器收到消息后调用）
	messageHandler := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		// 安全网关检查
		if err := gw.Check(ctx, msg); err != nil {
			return &adapter.Reply{Content: "安全检查未通过: " + err.Error()}, nil
		}
		return eng.Process(ctx, msg)
	}

	// Web WebSocket 适配器
	if cfg.Platforms.Web.Enabled {
		wa := webadapter.New()
		if err := wa.Start(ctx, messageHandler); err != nil {
			log.Printf("Web 适配器启动失败: %v", err)
		} else {
			srv.SetWebSocketHandler(wa.Handler())
			adapters = append(adapters, wa)
			fmt.Println("  WebSocket: ws://" + fmt.Sprintf("%s:%d/ws", cfg.Server.Host, cfg.Server.Port))
		}
	}

	// 飞书 Bot 适配器
	if cfg.Platforms.Feishu.Enabled {
		fa := feishu.New(cfg.Platforms.Feishu)
		if err := fa.Start(ctx, messageHandler); err != nil {
			log.Printf("飞书适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, fa)
			fmt.Println("  飞书 Webhook: http://0.0.0.0:6061/feishu/webhook")
		}
	}

	// Telegram Bot 适配器
	if cfg.Platforms.Telegram.Enabled {
		ta := telegram.New(cfg.Platforms.Telegram)
		if err := ta.Start(ctx, messageHandler); err != nil {
			log.Printf("Telegram 适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, ta)
			fmt.Println("  Telegram Bot: 长轮询模式")
		}
	}

	// 钉钉 Bot 适配器
	if cfg.Platforms.Dingtalk.Enabled {
		da := dingtalk.New(cfg.Platforms.Dingtalk)
		if err := da.Start(ctx, messageHandler); err != nil {
			log.Printf("钉钉适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, da)
			fmt.Println("  钉钉 Webhook: http://0.0.0.0:6062/dingtalk/webhook")
		}
	}

	// Discord Bot 适配器
	if cfg.Platforms.Discord.Enabled {
		da := discord.New(cfg.Platforms.Discord)
		if err := da.Start(ctx, messageHandler); err != nil {
			log.Printf("Discord 适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, da)
			fmt.Println("  Discord Bot: Gateway WebSocket 模式")
		}
	}

	// Slack Bot 适配器
	if cfg.Platforms.Slack.Enabled {
		sa := slack.New(cfg.Platforms.Slack)
		if err := sa.Start(ctx, messageHandler); err != nil {
			log.Printf("Slack 适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, sa)
			fmt.Println("  Slack Events: http://0.0.0.0:6063/slack/events")
		}
	}

	// 企业微信适配器
	if cfg.Platforms.Wecom.Enabled {
		wa := wecom.New(cfg.Platforms.Wecom)
		if err := wa.Start(ctx, messageHandler); err != nil {
			log.Printf("企业微信适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, wa)
			fmt.Println("  企业微信回调: http://0.0.0.0:6064/wecom/callback")
		}
	}

	// 微信公众号适配器
	if cfg.Platforms.Wechat.Enabled {
		wca := wechat.New(cfg.Platforms.Wechat)
		if err := wca.Start(ctx, messageHandler); err != nil {
			log.Printf("微信公众号适配器启动失败: %v", err)
		} else {
			adapters = append(adapters, wca)
			fmt.Println("  微信公众号回调: http://0.0.0.0:6065/wechat/callback")
		}
	}

	// 9. 监听退出信号，优雅关闭
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(sigCtx); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	fmt.Println("HexClaw 已启动")
	fmt.Printf("  Web UI: http://%s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("  健康检查: http://%s:%d/health\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("  聊天 API: POST http://%s:%d/api/v1/chat\n", cfg.Server.Host, cfg.Server.Port)

	// 等待退出信号或服务器错误
	select {
	case <-sigCtx.Done():
		log.Println("收到退出信号，正在关闭...")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("服务器异常: %w", err)
		}
	}

	// 优雅关闭
	shutdownCtx := context.Background()

	// 关闭平台适配器
	for _, a := range adapters {
		if err := a.Stop(shutdownCtx); err != nil {
			log.Printf("关闭 %s 适配器出错: %v", a.Name(), err)
		}
	}

	if err := srv.Stop(shutdownCtx); err != nil {
		log.Printf("关闭服务器出错: %v", err)
	}

	log.Println("HexClaw 已停止")
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

// markdownSkillWrapper 将 Markdown 技能适配为 skill.Skill 接口
//
// 桥接 marketplace.MarkdownSkill 和 skill.Skill 接口。
type markdownSkillWrapper struct {
	skill *marketplace.MarkdownSkill
}

func (w *markdownSkillWrapper) Name() string        { return w.skill.Name() }
func (w *markdownSkillWrapper) Description() string  { return w.skill.Description() }
func (w *markdownSkillWrapper) Match(content string) bool { return w.skill.Match(content) }

func (w *markdownSkillWrapper) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	result, err := w.skill.Execute(ctx, args)
	if err != nil {
		return nil, err
	}
	return &skill.Result{
		Content:  result.Content,
		Metadata: result.Metadata,
	}, nil
}
