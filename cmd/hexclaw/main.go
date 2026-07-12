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
	"encoding/base64"
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
	"sync"
	"syscall"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/spf13/cobra"

	"github.com/hexagon-codes/ai-core/llm"
	imagegen "github.com/hexagon-codes/ai-core/media/image"
	videogen "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/ai-core/media/voice"
	"github.com/hexagon-codes/ai-core/media/voicechat"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/events"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	genstore "github.com/hexagon-codes/toolkit/blobstore"

	"github.com/hexagon-codes/hexclaw/adapter"
	webadapter "github.com/hexagon-codes/hexclaw/adapter/web"
	"github.com/hexagon-codes/hexclaw/agents"
	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/audit"
	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/connector"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/desktop"
	"github.com/hexagon-codes/hexclaw/egress"
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
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/render"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12apihttp "github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	k12assembly "github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	k12engineadapter "github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	k12skilladapter "github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/secret"
	"github.com/hexagon-codes/hexclaw/security"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/hexclaw/webhook"
)

// 版本信息，通过 -ldflags 注入
var (
	version = "v0.4.8"
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

	// 桌面端=单用户自有机器：网关的多租户/合规类闸门全部放开（与宿主机语义一致），
	// 避免误杀/节流/掐预算影响功能。服务端（desktopMode=false）保持原配置不动。
	cfg.Security.RateLimit.RequestsPerMinute = 1_000_000 // 实质不限（pipeline 会把 0 强制回 60，故设极大值）
	cfg.Security.RateLimit.RequestsPerHour = 1_000_000
	cfg.Security.Cost.BudgetPerUser = 0             // 0 → 不挂成本预检层
	cfg.Security.Cost.BudgetGlobal = 0              // 0 → 不挂成本预检层
	cfg.Security.InjectionDetection.Enabled = false // 网关注入检测层
	cfg.Security.PIIRedaction.Enabled = false       // 不抹用户自己的手机号/邮箱
	cfg.Security.ContentFilter.Enabled = false      // 不按 harmful/illegal 拦内容
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
		security.SetDesktopMode(true) // 放行引擎/cron 的内容注入扫描（见 security/desktop_mode.go）
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
	checkDBHealth(cfg.Storage.SQLite.Path, desktopMode)
	store, err := sqlitestore.New(cfg.Storage.SQLite.Path)
	if err != nil {
		return fmt.Errorf("初始化存储失败: %w", err)
	}
	defer store.Close()

	// v0.4.0 方案 A：构造 Feature Flag Static 实例并注入 root ctx。
	// 产品级能力按功能优先默认开启；features: 段仅用于显式关闭或灰度覆盖。
	// 业务路径用 featureflag.Enabled(ctx, "name") 查询，未注册 flag 仍按配置错误处理为 OFF。
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
	cloudEgress := &egress.Policy{OnAudit: func(req egress.Request, decision egress.Decision) {
		logger.Info("[egress] 云出网判定", "purpose", req.Purpose, "data_class", req.DataClass,
			"audit_id", req.AuditID, "allow_cloud", decision.AllowCloud, "reason", decision.Reason)
	}}
	if err != nil {
		fmt.Printf("  ✗ LLM         跳过 (%v)\n", err)
	} else {
		router.SetEgressPolicy(cloudEgress)
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
		// 场景包出厂 seed（batteries-included·零下载）：K12 pack go:embed 的 skill 首启幂等注入
		// ~/.hexclaw/skills/（已存在不覆盖），须在 Init 前调用，本次启动即被扫描注册。
		if n, serr := mp.SeedFromFS(k12.BundledSkillsFS(), "skills"); serr != nil {
			logger.Warn("K12 场景包 skill 首启 seed 失败", "error", serr)
		} else if n > 0 {
			logger.Info("K12 场景包 skill 已出厂 seed", "count", n, "dir", cfg.Skills.Dir)
		}
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

	// 4.55 静态加密保险箱（主密钥 ~/.hexclaw/master.key，load-or-create）。提前到 MCP 连接之前
	// 创建：MCP server 的 env 凭证（DB 密码等）静态加密落盘，启动时须先解密再交给 mcpMgr 连接。
	// 加载失败降级为明文（保持可用），仅告警；绝不记录密钥或明文凭据。box 后续复用于平台实例 / connector。
	dataDir := filepath.Dir(cfg.Storage.SQLite.Path)
	var secretBox *secret.Box
	if box, berr := secret.LoadBox(dataDir); berr != nil {
		logger.Warn("[secret] 加载主密钥失败，凭据将以明文存储", "err", berr.Error())
	} else {
		secretBox = box
	}
	// 解密持久化的 MCP env（重启后从 yaml 读到 enc:v1:…），供下方 mcpMgr 连接使用。
	config.DecryptMCPEnv(cfg.MCP.Servers, secretBox)

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
					Env:       s.Env,
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
		// 用户经数据连接器授权的本地目录 → 注入 code_exec 沙箱只读放行（BUG-20260626）。
		SandboxReadablePaths: cfg.Skill.Sandbox.Filesystem.AllowedPaths,
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
	// BUG-20260710：write_file 落盘护栏——①拒绝把文本写成 .pdf/.docx 等二进制文档（坏文件），
	// 指引模型走产物导出/export_document；②写入成功后按 filesystem MCP 根解析绝对路径追加进结果，
	// 让模型能向用户说清文件真实位置（不再是"当前工作目录"黑话）。
	{
		var fsServerArgs [][]string
		for _, s := range cfg.MCP.Servers {
			if s.Enabled {
				fsServerArgs = append(fsServerArgs, s.Args)
			}
		}
		fsRoots := engine.FilesystemMCPRoots(fsServerArgs)
		toolExecutor.AddHook(engine.NewFileToolGuard(fsRoots)) // Before: 拒二进制文档扩展名（priority 5，先于审批）
		if mcpMgr != nil {
			// After: 按本次实际执行工具的 MCP owner 动态解析当前 roots；运行时
			// 重配立即生效，多 server/multi-root 不再拿启动快照猜路径。
			toolExecutor.AddHook(engine.NewFilePathHintResolver(mcpMgr.FilesystemRoots))
		}
	}

	// 6.1.1 接入权限审批 Hook (D24)
	// v0.4.3 §11.10 统一安全闸：PermissionPolicy 为单一权限闸（GA 默认 ON）。无人值守
	// 来源没有交互审批人，因此按 security.autonomy profile + 显式矩阵决定是否自动放行；
	// ActionDeny 仍优先，矩阵未命中则 fail-closed。
	permHub := engine.NewPermissionHub(60 * time.Second)

	// 6.1.0 自动化权限治理数据面：权限决策审计日志 + 任务级授权。
	// 初始化失败只降级（闸照常工作，少审计/grant），不阻断启动。
	var autonomyDecisions *autonomy.DecisionStore
	var autonomyGrants *autonomy.GrantStore
	if ds := autonomy.NewDecisionStore(store.DB()); ds != nil {
		if err := ds.Init(ctx); err != nil {
			fmt.Printf("  ⚠ Autonomy    决策审计初始化失败（降级为仅日志）: %v\n", err)
		} else {
			autonomyDecisions = ds
		}
	}
	if gs := autonomy.NewGrantStore(store.DB()); gs != nil {
		if err := gs.Init(ctx); err != nil {
			fmt.Printf("  ⚠ Autonomy    任务级授权初始化失败（降级为矩阵-only）: %v\n", err)
		} else {
			autonomyGrants = gs
		}
	}

	permOpts := []engine.PermissionHookOption{
		engine.WithCodeExecApproval(cfg.Skill.Builtin.CodeExecPolicy.CodeExecRequiresApproval()),
		engine.WithPolicy(engine.DefaultBaselinePolicy()),
		engine.WithSystemDispatchPolicy(engine.NewSystemDispatchPolicyFromConfig(cfg.Security.Autonomy)),
		engine.WithUnattendedReviewer(unattendedRiskAdapter{builtin.NewLLMRiskReviewer(eng.JudgeText)}),
	}
	if autonomyGrants != nil {
		permOpts = append(permOpts, engine.WithTaskGrants(autonomyGrants))
	}
	if autonomyDecisions != nil {
		permOpts = append(permOpts, engine.WithPermissionDecisionRecorder(autonomy.NewRecorder(autonomyDecisions)))
	}
	permHook := engine.NewPermissionHook(permHub, permOpts...)
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
	// 桌面端=单用户自有机器，不挂预算闸（agent 长任务/大 token 不被掐）；服务端照常。
	var budgetCtrl *engine.BudgetController
	if !desktopMode {
		budgetDuration := 30 * time.Minute
		if cfg.Budget.MaxDuration != "" {
			if d, err := time.ParseDuration(cfg.Budget.MaxDuration); err == nil {
				budgetDuration = d
			}
		}
		budgetCtrl = engine.NewBudgetController(engine.BudgetConfig{
			MaxTokens:   cfg.Budget.MaxTokens,
			MaxDuration: budgetDuration,
			MaxCost:     cfg.Budget.MaxCost,
		})
		eng.SetBudget(budgetCtrl)
		fmt.Printf("  ✓ Budget      max_tokens=%d, max_duration=%v, max_cost=$%.2f\n",
			cfg.Budget.MaxTokens, budgetDuration, cfg.Budget.MaxCost)
	}

	// 6.5 初始化知识库（向量搜索 + FTS5 混合检索）
	//
	// 分层架构:
	//   embedder: ai-core OpenAI Provider → hexagon rag/embedder 包装
	//   splitter: hexagon rag/splitter.RecursiveSplitter
	//   store:    hexclaw SQLite (文档元数据 + FTS5 + 向量 BLOB)
	kbOK := false
	var sharedEmbedder hexagon.VectorEmbedder // 共享 embedder: KB + VectorMemory + 语义搜索
	// 嵌入接线信息（BUG-20260712-B1）：装配完成后注入 api server，
	// 供 /knowledge/embedding-status 端点把「嵌入未就绪=自动注入休眠」可见化。
	var kbEmbedProvider, kbEmbedModel, kbEmbedBaseURL string
	var kbEmbedLocal bool
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
							// 自动发现（BUG-20260712-B1 嵌入开箱保证）：已装任一嵌入模型 → 直接用
							// （零配置零下载激活）；否则保持 nomic-embed-text 默认接线——用户经知识库页
							// 一键安装后无需重启即生效（Embed 按模型名打 Ollama，模型就位即成功）。
							if detected, ok := knowledge.DetectOllamaEmbeddingModel(ctx, pc.BaseURL); ok {
								embModel = detected
								logger.Info("[knowledge] 自动发现已安装的嵌入模型", "model", detected)
							} else {
								embModel = "nomic-embed-text"
								logger.Warn("[knowledge] 未发现已安装嵌入模型，默认接线 nomic-embed-text（安装前语义检索休眠，可在知识库页一键安装）")
							}
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
						kbEmbedProvider, kbEmbedModel = embProviderName, embModel
						kbEmbedBaseURL, kbEmbedLocal = pc.BaseURL, isOllama
					}
				}
			}

			if emb != nil {
				var guardedEmbedder hexagon.VectorEmbedder = emb
				if pc, ok := cfg.LLM.Providers[embProviderName]; ok && !isLocalEmbeddingProvider(embProviderName, pc) {
					// Guard the actual remote embedding boundary. The cache stays
					// outside it: a cache hit performs no network egress, while every
					// miss requires an explicit RAG purpose/data classification.
					guardedEmbedder = egress.NewCloudEmbedder(emb, cloudEgress)
				}
				// #2 Embedding 缓存：LRU 10000 + singleflight 防击穿，
				//    消除每次查询/重导都重打 embedding API 的成本与延迟。
				// #5 截断闸：入 embedding API 前按 rune 截断超长文本，
				//    防单条超长输入触发模型 token 超限错误/超量计费。
				//    truncating 置于 cache 外层，使缓存键作用于截断后文本。
				cached := hexagon.NewCachedEmbedder(guardedEmbedder) // 默认 LRU 10000
				sharedEmbedder = knowledge.NewTruncatingEmbedder(cached, 0)
			}
			// 2. 构造 splitter: MarkdownSplitter（#7 保留 header_path 结构元数据；
			//    对纯文本/无标题内容会自动按 chunkSize 退化为递归切分，无回归）
			chunkSize := cfg.Knowledge.ChunkSize
			if chunkSize <= 0 {
				chunkSize = 400
			}
			chunkOverlap := cfg.Knowledge.ChunkOverlap
			if chunkOverlap <= 0 {
				chunkOverlap = 80
			}
			sp := splitter.NewMarkdownSplitter(
				splitter.WithMarkdownChunkSize(chunkSize),
				splitter.WithMarkdownChunkOverlap(chunkOverlap),
				splitter.WithHeadersToSplit([]string{"#", "##", "###", "####"}),
				splitter.WithCodeBlockAware(true),
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
			// best-practice 检索开关（默认全开，可配关）
			hybridCfg.RerankEnabled = cfg.Knowledge.Rerank
			hybridCfg.ExpandEnabled = cfg.Knowledge.QueryExpand
			hybridCfg.ContextualEnabled = cfg.Knowledge.Contextual
			hybridCfg.MinScore = cfg.Knowledge.MinScore
			if cfg.Knowledge.CandidateK > 0 {
				hybridCfg.CandidateK = cfg.Knowledge.CandidateK
			}
			// #12 query/doc 嵌入非对称：显式配置优先，否则按模型智能默认
			//（nomic 系用其官方任务前缀 search_query/search_document；bge-m3/openai 无需前缀）
			qp, dp := cfg.Knowledge.Embedding.QueryPrefix, cfg.Knowledge.Embedding.DocPrefix
			if qp == "" && dp == "" && strings.Contains(strings.ToLower(embModel), "nomic") {
				qp, dp = "search_query: ", "search_document: "
			}
			hybridCfg.EmbedQueryPrefix = qp
			hybridCfg.EmbedDocPrefix = dp

			// 4. 创建 Manager (kbStore 同时实现 DocumentRepository + ChunkSearcher)
			// 注意: 传 sharedEmbedder（接口类型）而非 emb（*OpenAIEmbedder），
			// 避免 Go 接口 nil 陷阱（typed nil pointer 使接口非 nil 但 receiver 为 nil）
			mgrOpts := []knowledge.ManagerOption{
				knowledge.WithSplitter(sp),
				knowledge.WithHybridConfig(hybridCfg),
				// Bound each scheduled-task snapshot series so an @hourly collector
				// cannot grow the local KB without limit (IngestSnapshot prunes the
				// oldest past this cap).
				knowledge.WithSnapshotRetention(cfg.Knowledge.SnapshotRetention),
			}
			// #6/#8 注入 LLM（复用 Agent 的 LLM router）：重排 + 查询扩展。
			// router 为 nil 时不注入 → rerank/query-expand 自动降级关闭（安全）。
			if router != nil {
				// BUG-20260704：辅助 LLM 路由到本地单槽 provider 时跳过，避让前台主聊天（见 retrieval_llm.go）。
				mgrOpts = append(mgrOpts, knowledge.WithLLM(newRetrievalRerankLLM(router)))
				// 多模态入库：注入视觉转写器（router 的视觉模型给图片生成中文描述，再走文本 RAG 入库）。
				// router 为 nil 时不注入 → AddImageDocument 优雅报错而非吞入垃圾。
				mgrOpts = append(mgrOpts, knowledge.WithCaptioner(knowledge.CaptionerFunc(
					func(ctx context.Context, image []byte, mime string) (string, error) {
						ctx = egress.WithRequest(ctx, egress.PurposeVisionOCR, "", egress.ClassSensitiveMedia)
						provider, _, rErr := router.Route(ctx)
						if rErr != nil {
							return "", rErr
						}
						if mime == "" {
							mime = "image/png"
						}
						dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
						resp, cErr := provider.Complete(ctx, hexagon.CompletionRequest{
							Messages: []hexagon.Message{{
								Role: hexagon.RoleUser,
								MultiContent: []llm.ContentPart{
									llm.NewTextPart("请用中文客观、简洁地描述这张图片的主要内容（包含其中可见的文字），用于知识库检索。只输出描述本身。"),
									llm.NewImageURLPart(dataURL, "auto"),
								},
							}},
						})
						if cErr != nil {
							return "", cErr
						}
						return resp.Content, nil
					})))
			}
			// 专用 cross-encoder 重排：配置 rerank_model（或 SiliconFlow 自动）时，用
			// hexagon CohereReranker 指向同 provider 的 /rerank 端点，替代慢/贵的 LLM 重排。
			if pc, ok := cfg.LLM.Providers[embProviderName]; ok {
				rerankModel := cfg.Knowledge.RerankModel
				if rerankModel == "" && strings.Contains(strings.ToLower(pc.BaseURL), "siliconflow") {
					rerankModel = "BAAI/bge-reranker-v2-m3"
				}
				if rerankModel != "" && pc.APIKey != "" {
					rerankBase := strings.TrimSuffix(strings.TrimSuffix(pc.BaseURL, "/"), "/v1")
					cloudReranker := reranker.NewCohereReranker(pc.APIKey,
						reranker.WithCohereBaseURL(rerankBase),
						reranker.WithCohereModel(rerankModel),
						reranker.WithCohereTopK(hybridCfg.CandidateK))
					mgrOpts = append(mgrOpts, knowledge.WithDocReranker(guardedDocReranker{
						next: cloudReranker, guard: cloudEgress.GuardContext,
					}))
					logger.Info("[knowledge] 启用专用 cross-encoder 重排", "model", rerankModel)
				}
			}
			kbMgr := knowledge.NewManager(kbStore, kbStore, sharedEmbedder, mgrOpts...)
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
			// knowledge_search: opt-in SCOPED retrieval with metadata filters
			// (source / source_type / date). Complements the engine's automatic
			// whole-KB RAG injection for when the model wants narrowed recall.
			if err := skills.Register(builtin.NewKnowledgeSearchSkill(kbMgr)); err != nil {
				logger.Warn("[knowledge] failed to register knowledge_search skill", "err", err.Error())
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
	if cfg.FileMemory.Enabled {
		var err error
		fileMem, err = memory.New(memory.Options{
			Enabled:   true,
			Dir:       cfg.FileMemory.Dir,
			MaxMemory: cfg.FileMemory.MaxMemory,
			DailyDays: cfg.FileMemory.DailyDays,
		})
		if err != nil {
			fmt.Printf("  ✗ Memory      初始化失败: %v\n", err)
			fileMem = nil
		}
	}
	// bug#3a 2026-06-23：只要文件记忆系统创建成功就挂到引擎，不能用「启动时记忆是否为空」当闸门。
	// 否则首次启动（记忆为空）时不挂载，用户当次会话新增的记忆要等到下次重启才会注入 → 问答答不上。
	if fileMem != nil {
		eng.SetFileMemory(fileMem)
		fmt.Printf("  ✓ Memory      文件记忆 (%d 字符) + 自动记忆\n", len(fileMem.LoadContext()))
		// 增量 G①：配了 embedding 时为长期记忆召回接入向量化器 → hybrid（0.7 向量 + 0.3 BM25）。
		// 复用 KB 共享 embedder（已含 LRU 缓存 + 截断闸）；没配 embedding 则不接线，召回降级纯 BM25（行为不变）。
		if sharedEmbedder != nil {
			eng.SetMemoryEmbedder(sharedEmbedder)
			fmt.Println("  ✓ Memory      长期记忆 hybrid 召回 (向量 + BM25)")
		}
		// 增量 C：manage_memory 自管工具（AI 显式管理长期记忆：记住/更新/忘掉/置顶）。
		if err := skills.Register(builtin.NewManageMemorySkill(fileMem)); err != nil {
			fmt.Printf("  ✗ manage_memory 注册失败: %v\n", err)
		}
		// 增量 B：周期反思整合（默认关、opt-in）。零 LLM 确定性维护：去重 / 时序取代留史 / 晋升降级 / 归档陈旧。
		if cfg.FileMemory.Reflect {
			interval := time.Duration(cfg.FileMemory.ReflectIntervalMins) * time.Minute
			if cfg.FileMemory.Dreaming && router != nil {
				// 多阶段 dreaming（对标 OpenClaw）：light=机械反思（每 interval），deep=LLM 聚类合成留史（每 deep）。
				// 注入文本 LLM 整合器（复用 router）；nil router 时回退纯机械反思。
				fileMem.WithConsolidator(llmCompleteFunc(func(ctx context.Context, prompt string) (string, error) {
					ctx = egress.WithRequest(ctx, egress.PurposeGeneralChat, "", egress.ClassMemory)
					provider, _, rErr := router.Route(ctx)
					if rErr != nil {
						return "", rErr
					}
					resp, cErr := provider.Complete(ctx, hexagon.CompletionRequest{
						Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: prompt}},
					})
					if cErr != nil {
						return "", cErr
					}
					return resp.Content, nil
				}))
				deep := time.Duration(cfg.FileMemory.DreamingIntervalMins) * time.Minute
				stopDream := fileMem.StartDreaming(ctx, interval, deep)
				defer stopDream()
				// 回放相（engine 级，对标 Claude Dreaming「回放历史 session 提取模式」）：低频回放原始会话，
				// 复用抽取补「每轮在线抽取漏掉的」事实（memory=off 的轮 / inline+弱模型漏存 / 导入的旧会话）。
				// 单用户桌面：回放 desktop-user（= api.defaultDesktopUserID）的会话。
				stopReplay := eng.StartSessionReplay(ctx, "desktop-user", deep, deep*2)
				defer stopReplay()
				fmt.Printf("  ✓ Memory      多阶段 dreaming 已启用 (light 每 %d 分 / deep+回放 每 %d 分)\n",
					cfg.FileMemory.ReflectIntervalMins, cfg.FileMemory.DreamingIntervalMins)
			} else {
				stopReflect := fileMem.StartReflection(ctx, interval)
				defer stopReflect()
				fmt.Printf("  ✓ Memory      反思整合已启用 (每 %d 分钟)\n", cfg.FileMemory.ReflectIntervalMins)
			}
		}
		// 增量 G③：周期画像蒸馏（默认关、opt-in，deep 相）。低频把零碎事实 LLM 合成稳定用户画像 → Pinned 条。
		// 与机械反思并存不替换；prompt 强制只综合不杜撰。
		if cfg.FileMemory.Profile {
			syn := engine.NewProfileSynthesizer(eng)
			interval := time.Duration(cfg.FileMemory.ProfileIntervalMins) * time.Minute
			stopProfile := fileMem.StartProfileDistillation(ctx, interval, syn, memory.DistillProfileConfig{})
			defer stopProfile()
			fmt.Printf("  ✓ Memory      画像蒸馏已启用 (每 %d 分钟)\n", cfg.FileMemory.ProfileIntervalMins)
		}
		// 增量 G②：回复前主动会话深召回（默认开，仅 DM/交互式）。FTS-fast 零 LLM + 超时 + 熔断；
		// 把「该想起来」的旧上下文主动浮现，而非只等模型主动调 session_search。nil 配置=默认开。
		if cfg.FileMemory.ActiveRecall == nil || *cfg.FileMemory.ActiveRecall {
			eng.SetActiveRecall(engine.NewActiveRecall(store))
			fmt.Println("  ✓ Memory      主动会话召回已启用 (回复前 FTS 深召回)")
		}
	} else {
		fmt.Println("  ✗ Memory      未启用")
	}

	// 修缺陷G：接线 DB 维护安全阀（StartMaintenance / CleanupOldSessions 原本实现了却从未被调用=死代码）。
	// ① 周期 WAL checkpoint + 超阈值自动 VACUUM：纯维护、零数据丢失 → 无条件启用。
	stopMaint := store.StartMaintenance(ctx, cfg.Storage.SQLite.Path, time.Hour)
	defer stopMaint()
	fmt.Println("  ✓ Storage     DB 维护已启用 (WAL checkpoint + 超阈值自动 VACUUM)")
	// ② 会话保留清理：opt-in（session_retention_days>0 才删；个人桌面 app 默认永久保留历史）。
	if days := cfg.Storage.SQLite.SessionRetentionDays; days > 0 {
		cleanCtx, cancelClean := context.WithCancel(ctx)
		defer cancelClean()
		runCleanup := func() {
			if n, err := store.CleanupOldSessions(cleanCtx, days); err == nil && n > 0 {
				fmt.Printf("  ✓ Storage     已清理过期会话 %d 个 (保留 %d 天)\n", n, days)
			}
		}
		runCleanup() // 启动即清一次，其后每日一次
		go func() {
			t := time.NewTicker(24 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-cleanCtx.Done():
					return
				case <-t.C:
					runCleanup()
				}
			}
		}()
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
	srv.SetKnowledgeEmbeddingInfo(api.KnowledgeEmbeddingInfo{
		Enabled: cfg.Knowledge.Enabled, Provider: kbEmbedProvider, Model: kbEmbedModel,
		BaseURL: kbEmbedBaseURL, Local: kbEmbedLocal,
	})
	// 嵌入模型首启静默预置（BUG-20260712-B1 三态机制：成功=用户零感知；失败=知识库页
	// 浮手动重试横幅；可经 knowledge.embedding.disable_auto_install 关闭——计费网络逃生口）。
	// 后台 goroutine 不阻塞启动；Ensure 幂等（已装 no-op），Embed 按模型名打 Ollama，
	// 模型就位即生效无需重启。
	if cfg.Knowledge.Enabled && kbEmbedLocal && kbEmbedModel != "" && !cfg.Knowledge.Embedding.DisableAutoInstall {
		go func() {
			api.SetKnowledgeEmbeddingPulling(true)
			defer api.SetKnowledgeEmbeddingPulling(false)
			pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Hour)
			defer pcancel()
			if ok, err := knowledge.EnsureOllamaEmbeddingModel(pctx, kbEmbedBaseURL, kbEmbedModel); err != nil {
				logger.Warn("[knowledge] 嵌入模型静默预置失败（知识库页可手动安装）", "model", kbEmbedModel, "error", err)
			} else if ok {
				logger.Info("[knowledge] 嵌入模型已就位，语义检索激活", "model", kbEmbedModel)
			}
		}()
	}
	srv.SetVersion(version)
	// 自动化权限治理 API（Profile 热更 / 预检 / 决策日志 / 任务级授权）
	srv.SetAutonomy(permHook, autonomyDecisions, autonomyGrants, configFile)

	// §11.8 交互层：Prompt 库（服务端下发 + CRUD）。
	// 砍薄版（§5）：旧记忆薄版 library.MemoryStore + /api/v1/memories 已并入统一文件记忆；
	// 升级首启时一次性迁移历史 memories 表 → FileMemory（standing→rule / fact→fact），迁移后删表（幂等）。
	promptStore := library.NewPromptStore(store.DB())
	srv.SetPromptStore(promptStore)
	if fileMem != nil {
		if n, mErr := memory.MigrateLegacyMemories(ctx, store.DB(), fileMem); mErr != nil {
			fmt.Printf("  ⚠ Memory      旧记忆薄版迁移失败（不阻断启动）: %v\n", mErr)
		} else if n > 0 {
			fmt.Printf("  ✓ Memory      旧记忆薄版已迁移 %d 条 → 统一文件记忆\n", n)
		}
	}

	// 8.0.1 接入沙箱网络热更新 (Bug2 修复)
	if skillDeps.CodeExecSkill != nil {
		srv.SetSandboxCallbacks(skillDeps.CodeExecSkill.UpdateNetwork, skillDeps.CodeExecSkill.NetworkEnabled)
	}
	if skillDeps.CodeExecSkill != nil || skillDeps.FileAccess != nil {
		srv.SetSandboxAllowedPathsCallback(func(paths []string) error {
			if skillDeps.CodeExecSkill != nil {
				if err := skillDeps.CodeExecSkill.UpdateReadablePaths(paths); err != nil {
					return err
				}
			}
			if skillDeps.FileAccess != nil {
				skillDeps.FileAccess.UpdateAllowedPaths(paths)
			}
			return nil
		})
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
	if fileMem != nil {
		lc.Info("memory", fmt.Sprintf("文件记忆已加载 (%d 字符) — 跨会话长期记忆", len(fileMem.LoadContext())))
	}
	// Note: "已就绪" 日志迁移到 onReady 回调，仅在 HTTP 端口真实 bind 成功后才打印。

	// 挂载预算控制器 API（桌面端无预算闸时跳过）
	if budgetCtrl != nil {
		srv.SetBudgetController(budgetCtrl)
	}

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
					// The stable snapshot base title (job.Name) is stamped by the
					// scheduler's runAgentJob before this runner is called, so any
					// knowledge_ingest in this round writes a coherent series — that
					// contract now lives in (and is tested by) the cron package.
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
						// kb_ingest is cron-exclusive, so every Starlark script
						// write is a scheduled snapshot: append a new document
						// (never overwrite the previous run), skip the write when
						// content is unchanged, and keep a bounded series. `title`
						// is the script's stable base title for the series.
						doc, _, err := kb.IngestSnapshot(ingestCtx, title, content, source)
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
					// webhook_id 供 engine 盖任务身份（task_ref=webhook:<id>）：
					// 任务级 grant 求值与权限决策审计归因都依赖它。
					Metadata: map[string]string{"source": "webhook", "webhook": event.WebhookName, "webhook_id": event.WebhookID},
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
		cfgWriter := config.NewWriter(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
		cfgWriter.SetSecretBox(secretBox) // MCP env 凭证静态加密落盘（保险箱接管 MCP 凭证）
		srv.SetCfgWriter(cfgWriter)
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
				Temperature:  ac.Temperature, // 指针语义直透（nil=未设，显式 0=确定性；P2-4）
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
			ctx = egress.WithRequest(ctx, egress.PurposeGeneralChat, "", egress.ClassGeneral)
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
	// P1/#7 能力路由自省：让主 Agent 先看清有哪些专门 Agent 再派，避免盲派不存在的角色名。
	if err := skills.Register(engine.NewListAgentsSkill(agentRouter)); err != nil {
		logger.Error("注册 ListAgentsSkill 失败", "error", err)
	}
	// 子 Agent 注册表（运行时纵深骨架：角色/工具继承/持久化/session/续接）。
	subagentRegistry := engine.NewSubAgentRegistry(engine.DefaultSubAgentRegistryFile())
	srv.SetSubAgentRegistry(subagentRegistry)

	// orchestrate 并发自适应「LLM 后端」而非 CPU 核心：HexClaw 子 Agent 干的是 LLM 调用，
	// 瓶颈在 provider rate-limit/成本（云端）或本机模型吞吐（本地，同机扛不住多并发推理）。
	// 默认 provider 是本地 → 并发收到 2；云端 → 放到 8。
	if defaultProviderIsLocal(cfg) {
		engine.SetMaxOrchestrateConcurrency(2)
	} else {
		engine.SetMaxOrchestrateConcurrency(8)
	}

	// #4 reduce 合成：fan-out 后用 synthesizer 把子产出归并 + 冲突检测，替代裸拼接。同样按 LLM
	// 后端自适应——云端模型归并质量高、值这一次额外调用 → 开；本地模型慢且归并参差 → 关（裸拼接）。
	engine.SetOrchestrateSynthesis(!defaultProviderIsLocal(cfg))

	// #5 supervisor 反馈环：云端放开迭代轮数上限（首轮 + 至多 2 轮补派）；本地模型出结构化 JSON 决策
	// 不可靠（监工调用易空耗一次推理后 fail-parse），收到 1 = 等同关闭迭代，永远单轮。
	if defaultProviderIsLocal(cfg) {
		engine.SetMaxSupervisorRounds(1)
	} else {
		engine.SetMaxSupervisorRounds(3)
	}

	// OrchestrateSkill + SpawnSkill 共享 executor: 通过 engine.Process 执行子任务。
	// spec 携带 role/source/spawn_depth/工具继承/run_id/session，由 ApplySpecToMessage 落到 metadata。
	agentExecFn := func(ctx context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		msg := &adapter.Message{
			ID:       "sub-" + idgen.NanoID(),
			Platform: adapter.PlatformAPI,
			UserID:   "system",
			Content:  spec.Task,
		}
		engine.ApplySpecToMessage(msg, spec)
		reply, err := eng.Process(ctx, msg)
		if err != nil {
			return engine.SubAgentResult{}, err
		}
		// msg.SessionID 经 Process 解析后即子会话 id；session-mode 回传供后续续聊。
		return engine.SubAgentResult{Output: reply.Content, SessionID: msg.SessionID}, nil
	}
	if err := skills.Register(engine.NewOrchestrateSkill(agentExecFn, subagentRegistry)); err != nil {
		logger.Error("注册 OrchestrateSkill 失败", "error", err)
	}
	if err := skills.Register(engine.NewSpawnSkill(agentExecFn, subagentRegistry)); err != nil {
		logger.Error("注册 SpawnSkill 失败", "error", err)
	}
	// P0（K12 正确性）：solve = 带 code_exec 独立验证的解题工具。verifier 子 Agent 只许 code_exec、
	// fresh-context 重算，把"算术幻觉"这一 K12 头号错因用执行 grounding 兜住。
	solveSkill := engine.NewSolveSkill(agentExecFn, subagentRegistry)
	if err := skills.Register(solveSkill); err != nil {
		logger.Error("注册 SolveSkill 失败", "error", err)
	}

	// 文档渲染服务（提前构建到函数作用域，K12 导出与平台 export skill 复用同一实例，避免 sandbox/cache 冲突）。
	var renderSvc *render.Service
	if rs, rErr := buildRenderService(); rErr == nil {
		renderSvc = rs
	} else {
		logger.Warn("[warn] 文档渲染服务未启用", "error", rErr)
	}

	// K12 场景包装配（v0.5.0）：六缝注册 + records 存储 + 真 adapter（solve/识题/学情/教材/档案/渲染）+ 用例，挂 /api/k12/。
	// AP-1：K12 只经 scenarios/k12 通过 registry 注入；平台 engine/api 不认识 K12。
	{
		// 识题视觉闭包：作业图片 → 云端 vision 文本（mirror knowledge captioner），出网前过 EgressPolicy。
		visionFn := func(ctx context.Context, image []byte, prompt string) (string, error) {
			ctx = egress.WithRequest(ctx, egress.PurposeVisionOCR, "", egress.ClassSensitiveMedia)
			if router == nil {
				return "", fmt.Errorf("未配置视觉模型")
			}
			// BUG-20260712：识题用**配置的默认**视觉模型（尊重「设置哪个模型走哪个」），不走
			// cost-aware（那会抓本地免费 provider、无视用户配的 glm-4v-flash，既慢又曾 404）。
			provider, visionModel, rErr := eng.RouteForVision(ctx)
			if rErr != nil {
				logger.Warn("[k12识题] 视觉模型路由失败", "err", rErr.Error(), "image_bytes", len(image))
				return "", rErr
			}
			// 排查用：打印识题实际选中的 provider/model + egress 用途，一眼定位路由/出网问题。
			logger.Info("[k12识题] 视觉模型已路由", "provider", provider.Name(), "model", visionModel,
				"image_bytes", len(image), "egress", "vision_ocr[sensitive_media]")
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
			resp, cErr := provider.Complete(ctx, hexagon.CompletionRequest{
				Messages: []hexagon.Message{{
					Role: hexagon.RoleUser,
					MultiContent: []llm.ContentPart{
						llm.NewTextPart(prompt),
						llm.NewImageURLPart(dataURL, "auto"),
					},
				}},
			})
			if cErr != nil {
				logger.Warn("[k12识题] 视觉模型调用失败", "provider", provider.Name(), "model", visionModel, "err", cErr.Error())
				return "", cErr
			}
			return resp.Content, nil
		}

		k12Opts := []k12assembly.Option{
			k12assembly.WithRecognizer(k12engineadapter.NewRecognizerAdapter(visionFn)),
		}
		if fileMem != nil {
			k12Opts = append(k12Opts, k12assembly.WithInsights(k12engineadapter.NewInsightsAdapter(fileMem)))
		}
		if kb := eng.KnowledgeBase(); kb != nil {
			k12Opts = append(k12Opts, k12assembly.WithGrounding(k12engineadapter.NewGroundingAdapter(kb)))
		}
		// 建档/改档：接 agent 路由 + 持久化（读改写 agents.metadata）。
		k12Opts = append(k12Opts,
			k12assembly.WithProfiles(k12engineadapter.NewProfileAdapter(agentRouter, agentStore)),
			k12assembly.WithArchiveRestorer(func(recordStore *records.Store) k12usecase.ArchiveRestorer {
				return k12engineadapter.NewArchiveRestoreAdapter(store.DB(), recordStore, agentRouter, agentStore)
			}),
		)
		// 导出 PDF/Word：接 render 服务（nil 时 /export 优雅降级 markdown）。
		if renderSvc != nil {
			k12Opts = append(k12Opts, k12assembly.WithRenderer(k12engineadapter.NewRenderAdapter(renderSvc)))
		}

		k12Solve := classifiedSolveExecutor{next: solveSkill}
		if k12rt, k12err := k12assembly.Wire(store.DB(), k12Solve, k12Opts...); k12err != nil {
			logger.Error("装配 K12 场景包失败", "error", k12err)
		} else {
			// IM 入站错题入库副作用：把 K12 批改闭环包成通用 skill 注入工具面。
			// engine 只见通用工具（守 AP-1）；辅导 Agent 在群里被路由命中时，LLM 调
			// k12_grade 即跑完整批改+错题入库+学情，实例 scope 从 ctx 的已路由 Agent 取。
			// 辅导 Agent 模板须在 Skills 声明 "k12_grade"（建档时挂载）。
			if err := skills.Register(k12skilladapter.NewGradeSkill(k12rt.Deps)); err != nil {
				logger.Warn("注册 k12_grade skill 失败", "error", err)
			}
			// 复习飞轮「读」侧：与 k12_grade（写）对称。LLM 在对话/群里被家长要求"复习
			// 错题"时调 k12_review，取到期错题队列 + 陪练方案（守答案遮罩）。此前 review
			// 用例只经 HTTP + cron 暴露，自由对话读不到错题本——本工具补上这个触达缺口。
			// 辅导 Agent 模板须在 Skills 声明 "k12_review"（建档时挂载）。
			if err := skills.Register(k12skilladapter.NewReviewSkill(k12rt.Deps)); err != nil {
				logger.Warn("注册 k12_review skill 失败", "error", err)
			}
			// 自动化沉淀「调度」缝：注入平台 cron.Scheduler 包成 CronRegistrar，
			// POST /api/k12/cron/provision 即可为实例注册默认任务（周卷/日提醒/月报/学期确认）。
			var k12Cron k12apihttp.CronRegistrar
			if scheduler != nil {
				k12Cron = k12CronRegistrar{sched: scheduler}
			}
			// IM 入站路由「绑定」缝：POST /api/k12/bind-im 把家庭群绑到辅导实例。
			k12Binder := &k12IMBinder{router: agentRouter, store: agentStore}
			k12Base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port)
			srv.Mount("/api/k12", k12apihttp.NewHandler(k12apihttp.Runtime{
				Views:   k12rt.Registry.Views,
				Records: k12rt.Records,
				Deps:    k12rt.Deps,
				Cron:    k12Cron,
				Binder:  k12Binder,
				BaseURL: k12Base,
			}))
			// 清债 P5：engine 的 agent-mode 路由消费场景包 mode 特性（K12 领域词不再 engine 硬编码）。
			modes := k12rt.Registry.Modes
			engine.SetModeKeywordMatcher(func(mode engine.AgentMode, text string) bool {
				return modes.MatchesMode(string(mode), text)
			})
			// 清债 P5：engine 的交互按钮消费场景包 ButtonProvider（识题确认按钮内容不再 engine 硬编码）。
			buttons := k12rt.Registry.Buttons
			engine.SetInteractiveButtonProvider(func(md map[string]string) *adapter.InteractivePayload {
				b, ok := buttons.Match(func(key string) bool { return engine.ShouldEnrichTrigger(md, key) })
				if !ok {
					return nil
				}
				payload := &adapter.InteractivePayload{Type: adapter.InteractiveTypeButtons, Prompt: b.Prompt}
				for i, label := range b.Labels {
					action := ""
					if i < len(b.Actions) {
						action = b.Actions[i]
					}
					variant := adapter.ButtonSecondary
					if i == 0 {
						variant = adapter.ButtonPrimary
					}
					payload.Buttons = append(payload.Buttons, adapter.InteractiveButton{Label: label, Action: action, Variant: variant})
				}
				return payload
			})
			logger.Info("K12 场景包已装配", "mount", "/api/k12", "识题", true, "学情", fileMem != nil, "教材grounding", eng.KnowledgeBase() != nil)
		}
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
				default:
					// 不再静默丢弃：未识别的 chain provider 显式告警（避免"配了没生效"无感知）。
					// 付费 provider（openai-tts/azure）在 chain 中的完整接入为后续 feature；
					// 桌面默认走免费 edge-tts，不受影响。
					fmt.Printf("  ⚠️  Voice TTS chain: 跳过未识别的 provider %q（chain 目前支持 minimax/edge）\n", name)
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
			desktopSvc.NotifySource(title, body, nt, "cron")
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
	// 复用前面提前构建的 renderSvc（K12 导出与本处 export skill / /api/v1/render 共用一实例）。
	if renderSvc != nil {
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
	if disabled := parseDisabledIMProviders(os.Getenv("HEXCLAW_DISABLE_IM")); len(disabled) > 0 {
		instanceMgr.SetDisabledProviders(disabled...)
		logger.Warn("[instances] IM provider startup disabled by HEXCLAW_DISABLE_IM", "providers", strings.Join(disabled, ","))
	}
	// secretBox / dataDir 已在 MCP 连接前统一创建（见 4.55）。这里仅把 box 注入平台实例管理器，
	// 让 IM config_json 与 MCP env、connector token 共用同一把主密钥静态加密。
	if secretBox != nil {
		instanceMgr.SetSecretBox(secretBox)
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
	// IM 入站回执：所有平台实例共用同一 handler，这里统一包一层，让
	// 「我不在时 IM 来消息了」进入桌面通知中心。桌面 chat 经 wa（Web 适配器）
	// 直连 messageHandler，不走 instanceMgr，故不会被此回执波及。
	instanceMgr.SetHandler(func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		reply, err := messageHandler(ctx, msg)
		// IM 端无结构化工具卡：把本轮工具活动追加为紧凑文本尾注，让 Telegram/企业微信/飞书等
		// 用户也能看到「调用了什么工具、成没成、结果摘要」。桌面经 wa 直连 messageHandler、
		// 不走此 handler，故工具卡体验不受影响（避免桌面气泡重复展示）。
		if reply != nil && len(reply.ToolCalls) > 0 {
			reply.Content += adapter.ToolCallDigest(reply.ToolCalls)
		}
		title := string(msg.Platform) + " 新消息"
		if msg.UserName != "" {
			title = msg.UserName + " · " + string(msg.Platform)
		}
		if err != nil {
			desktopSvc.NotifySource(title, "消息处理失败: "+err.Error(), desktop.NotifyWarning, "im")
		} else {
			desktopSvc.NotifySource(title, clipText(msg.Content, 120), desktop.NotifyInfo, "im")
		}
		return reply, err
	})
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

	// 记忆检索双通道之「会话深召回」：session_search 让 Agent 按关键词翻原始历史会话（FTS 兜底），
	// 与策展事实注入互补（方案 §6bis.B / §7.2）。
	if store != nil {
		if err := skills.Register(builtin.NewSessionSearchSkill(store)); err != nil {
			logger.Warn("[memory] failed to register session_search skill", "err", err.Error())
		} else {
			fmt.Println("  ✓ Skill       session_search (会话深召回)")
		}
	}

	// P0 自感知：注入 AppIntrospector（系统名片：基数 + 版本 + 工具能力提示）+ 注册 app_query 只读工具。
	// 闭包持有各 Manager（任一 nil 优雅降级）；所有读返回**脱敏**（不含凭据，红线见 appIntrospectorImpl 注释）。
	appIntrospector := &appIntrospectorImpl{
		version:     version,
		cfg:         cfg,
		mcpMgr:      mcpMgr,
		scheduler:   scheduler,
		webhookMgr:  webhookMgr,
		agentRouter: agentRouter,
		instanceMgr: instanceMgr,
		logs:        srv.LogCollector(),
		srv:         srv,
	}
	eng.SetAppIntrospector(appIntrospector)
	if err := skills.Register(builtin.NewAppQuerySkill(appIntrospector)); err != nil {
		logger.Warn("[app_query] failed to register app_query skill", "err", err.Error())
	} else {
		fmt.Println("  ✓ Skill       app_query (P0 自感知：连接/MCP/cron/webhook/agents/config/logs 只读·脱敏·fence)")
	}
	// P1 自愈：白名单可逆写操作（cron retry/resume/pause），由 PermissionPolicy heal-approve 强制审批。
	if err := skills.Register(builtin.NewAppHealSkill(scheduler, srv)); err != nil {
		logger.Warn("[app_heal] failed to register app_heal skill", "err", err.Error())
	} else {
		fmt.Println("  ✓ Skill       app_heal (P1 建议+审批后自愈：cron retry/resume/pause·可逆)")
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

			// 接通桌面通知中心 → 前端实时推送: 任何 desktopSvc.Notify（cron 任务
			// 完成/失败、IM 入站回执、heal 结果等）都经此广播给桌面 WS 客户端。
			// content=body，结构化字段走 metadata，前端按 source 映射图标与深链。
			desktopSvc.SetNotifyCallback(func(n desktop.Notification) {
				wa.Broadcast("desktop_notification", n.Body, map[string]string{
					"id":     n.ID,
					"title":  n.Title,
					"type":   string(n.Type),
					"source": n.Source,
				})
			})
		}
	}
	if err := instanceMgr.StartEnabled(ctx); err != nil {
		return fmt.Errorf("启动平台实例失败: %w", err)
	}

	// 本地模型后台预热（BUG-20260710）：默认路由是 ollama/local 时，把巨型 system prompt
	// +工具模板先行 prefill 进 KV 缓存（纯 CPU 机型冷路径实测 344s），用户首条消息走热路径。
	// 不阻塞启动、失败仅告警；云端默认路由自动跳过。
	// BUG-20260710-H2：必须在**全部装配完成后**启动（SetAgentRouter/SetModeKeywordMatcher/
	// solve·k12 skill 注册都在上方）——早启会与装配 setter 数据竞争，且预热采到的工具集
	// 比真实首问少几个工具，KV 前缀分叉、预热白做。
	eng.StartLocalWarmup(ctx)

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

// clipText 截断字符串到至多 max 个 rune，超出补省略号；用于通知正文摘要。
func clipText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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

// k12IMBinder 把平台 router.Dispatcher + store 包成 K12 的 IMBinder 缝（AP-1：K12 不 import router）。
// 绑定 = 内存路由规则（即时生效）+ 持久化（重启存活），chat 级绑定（PRD §3.1.7 各绑各的群）。
type k12IMBinder struct {
	router *agentrouter.Dispatcher
	store  *agentrouter.SQLiteStore
	mu     sync.Mutex
}

func (b *k12IMBinder) Bind(ctx context.Context, platform, instanceID, chatID, agentName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rule := agentrouter.Rule{
		Platform:   platform,
		InstanceID: instanceID,
		ChatID:     chatID,
		AgentName:  agentName,
		Priority:   50, // 群级显式绑定，优先于平台默认
	}
	return b.router.ReplaceRulePersisted(rule, func(persisted *agentrouter.Rule) error {
		if b.store == nil {
			return nil
		}
		return b.store.ReplaceRuleScope(ctx, persisted)
	})
}

// k12CronRegistrar 把平台 cron.Scheduler 包成 K12 的 CronRegistrar 缝（AP-1：K12 不 import cron）。
// 用 AddJobFromScript 直接喂 K12 产的确定性 Starlark 脚本，跳过 LLM 编译。
type k12CronRegistrar struct{ sched *cron.Scheduler }

// classifiedSolveExecutor is the composition boundary between K12 profile data
// and the LLM-backed solve tool. The remote-provider facade performs the actual
// policy decision immediately before each network attempt; this wrapper only
// carries explicit semantics there (avoiding duplicate audits and preserving
// local-provider bypass).
type classifiedSolveExecutor struct {
	next k12engineadapter.SolveExecutor
}

func (e classifiedSolveExecutor) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	ctx = egress.WithRequest(ctx, egress.PurposeSolveVerify, "",
		egress.ClassGeneral, egress.ClassSensitiveProfile)
	if e.next == nil {
		return nil, fmt.Errorf("k12 solve executor 未注入")
	}
	return e.next.Execute(ctx, args)
}

func (r k12CronRegistrar) Register(ctx context.Context, kind string, spec k12usecase.CronSpec, platform, chatID, userID string) (string, error) {
	// BUG-20260710-H3：接口契约「幂等键 = agent+kind」。kind 必须真正参与
	// 校验，防止调用方把一个任务类别意外覆盖到另一个类别的稳定 key 上。
	kind = strings.TrimSpace(kind)
	key := strings.TrimSpace(spec.Key)
	if kind == "" || key == "" || !strings.HasSuffix(key, "/"+kind) {
		return "", fmt.Errorf("k12 cron 幂等键与 kind 不匹配: key=%q kind=%q", key, kind)
	}
	req := cron.AddJobRequest{
		Name:      spec.Name,
		Schedule:  spec.Schedule,
		UserID:    userID,
		Platform:  platform,
		ChatID:    chatID,
		Deliver:   spec.Deliver,
		SourceKey: key,
	}
	job, err := r.sched.UpsertJobFromScript(ctx, req, spec.Runtime, spec.Script)
	if err != nil {
		return "", fmt.Errorf("k12 cron 原子覆盖（旧任务保持不变）: %w", err)
	}
	return job.ID, nil
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

// defaultProviderIsLocal 判定默认 LLM provider 是否为本地（Ollama 类）：base_url 指向 localhost。
// 用于 orchestrate 并发自适应——本地模型同机扛不住多并发推理，并发应收到 2。
func defaultProviderIsLocal(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	p, ok := cfg.LLM.Providers[cfg.LLM.Default]
	if !ok {
		return false
	}
	url := strings.ToLower(p.BaseURL)
	return strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") || strings.Contains(url, "11434")
}

func isLocalEmbeddingProvider(name string, provider config.LLMProviderConfig) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.TrimSpace(provider.BaseURL) != "" {
		return llmrouter.IsLocalProviderBaseURL(provider.BaseURL)
	}
	return strings.Contains(name, "ollama")
}

// llmCompleteFunc 把 router 文本补全闭包适配为 memory.ConsolidateLLM（dreaming 深相整合用）。
type llmCompleteFunc func(ctx context.Context, prompt string) (string, error)

// Complete 实现 memory.ConsolidateLLM。
func (f llmCompleteFunc) Complete(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

func parseDisabledIMProviders(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	all := []string{"dingtalk", "feishu", "discord", "telegram", "slack", "wechat", "wecom", "line", "whatsapp", "matrix", "email"}
	if strings.EqualFold(raw, "all") || strings.EqualFold(raw, "true") || raw == "1" {
		return all
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
