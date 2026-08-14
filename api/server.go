// Package api 提供 HexClaw HTTP API 服务
//
// 包含以下端点：
//   - GET    /health                            健康检查
//   - POST   /api/v1/chat                       同步聊天
//   - GET    /api/v1/sessions                   会话列表
//   - GET    /api/v1/sessions/{id}              会话详情
//   - POST   /api/v1/sessions/{id}/suggest-title 自动生成会话标题
//   - DELETE /api/v1/sessions/{id}              删除会话
//   - GET    /api/v1/sessions/{id}/messages     消息历史
//   - GET    /api/v1/sessions/{id}/branches     会话分支列表
//   - POST   /api/v1/sessions/{id}/fork         创建对话分支
//   - GET    /api/v1/sessions/{id}/checkpoints  检查点列表
//   - GET    /api/v1/messages/search            全文搜索消息
//   - DELETE /api/v1/messages/{id}              删除单条消息
//   - GET    /api/v1/budget/status              预算使用状态
//   - GET    /api/v1/tools/cache/stats          工具缓存统计
//   - GET    /api/v1/tools/metrics              工具调用指标
//   - GET    /api/v1/tools/permissions          工具权限规则
//
// 服务器支持优雅关闭：收到 SIGINT/SIGTERM 后等待请求处理完毕再退出。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"

	imagegen "github.com/hexagon-codes/ai-core/media/image"
	videogen "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/ai-core/media/voice"
	"github.com/hexagon-codes/ai-core/media/voicechat"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/connector"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/desktop"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/gateway"
	"github.com/hexagon-codes/hexclaw/instances"
	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/library"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/render"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/hub"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/hexclaw/streamstate"
	"github.com/hexagon-codes/hexclaw/webhook"
	genstore "github.com/hexagon-codes/toolkit/blobstore"
	"github.com/hexagon-codes/toolkit/net/sse"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Server HTTP API 服务器
type Server struct {
	cfg *config.Config
	// cfgMu 串行化 s.cfg 的 read-copy-save-apply 写路径（GO-7/BUG-20260703）：
	// 各配置写 handler 都做「整结构浅拷贝→落盘→回写」，无锁时既有同址读写
	// 竞争（拷贝读 vs 字段写），也有 lost-update（旧副本落盘抹掉他人变更）。
	cfgMu              sync.RWMutex
	engine             engine.Engine
	gateway            gateway.Gateway
	store              storage.Store                 // 数据存储层
	// sessionDeletedHook 在会话删除成功（durable 撤销已提交）后回调，供
	// PermissionHub 等进程内状态清理使用；hook 缺失或失败不改变删除结果。
	sessionDeletedHook func(sessionID string)
	kb                 *knowledge.Manager            // 知识库管理器（可选）
	semanticIndex      SemanticIndexAPI              // corpus 级语义索引策略/持久 Job（可选）
	webhookMgr         *webhook.Manager              // Webhook 管理器（可选）
	scheduler          *cron.Scheduler               // Cron 调度器（可选）
	promptStore        *library.PromptStore          // §11.8 Prompt 库（可选）
	fileMem            *memory.FileMemory            // 文件记忆（可选）
	vectorMem          *memory.VectorMemory          // 向量语义记忆（可选）
	mcpMgr             *hexmcp.Manager               // MCP 管理器（可选）
	mp                 *marketplace.Marketplace      // 技能市场（可选）
	skillHub           *hub.Hub                      // 在线技能市场（可选）
	agentRouter        *router.Dispatcher            // 多 Agent 路由器（可选）
	agentStore         router.Store                  // Agent/Rule 持久化（可选）
	agentMetadataGuard func(map[string]string) error // 场景 metadata capability guard（可选）
	agentResources     AgentResourceCleaner          // Agent 归属资源删除 saga（可选）
	instanceMgr        *instances.Manager            // 平台实例运行时（可选）
	connectorStore     *connector.Store              // 数据连接器(GitHub/Notion 只读，token 加密)（可选）
	canvasSvc          *canvas.Service               // Canvas/A2UI 服务（可选）
	voiceSvc           *voice.Service                // 语音服务（可选）
	voiceChatSvc       *voicechat.Service            // 语音对话服务（可选）
	imagegenSvc        *imagegen.Service             // 图像生成服务（可选）
	videogenSvc        *videogen.Service             // 视频生成服务（可选）
	renderSvc          *render.Service               // 文档渲染服务（可选）
	capabilities       *llmrouter.CapabilityService  // A7 模型 tool_call 能力探测（可选）
	genStore           *genstore.Store               // 生成内容持久化（图像/视频）
	kbEmbedding        *KnowledgeEmbeddingInfo       // 知识库嵌入接线信息（BUG-20260712-B1，可选）
	reloadGenServices  func()                        // LLM 配置变更后重建 gen 服务（main.go 注入）
	// reloadSemanticRuntime builds and atomically installs the next embedding
	// resolver/registry generation while draining the prior gate. The legacy
	// invalidator remains for non-desktop callers and compatibility tests.
	reloadSemanticRuntime     func(context.Context, config.LLMConfig) error
	invalidateSemanticRuntime func(context.Context) error
	desktopSvc                *desktop.Service             // 桌面集成服务（可选）
	cfgWriter                 *config.Writer               // 配置文件写入器（MCP 持久化用）
	wsHandler                 http.Handler                 // WebSocket Handler（可选）
	sidecarCapabilityToken    string                       // Desktop 每次启动注入的 loopback-only capability（可选）
	credentialResolver        *inMemoryCredentialResolver  // 原生协调器引用 -> 进程内更新候选
	attachmentStaging         *attachmentStagingStore      // owner-bound ephemeral binary receipts
	extraMounts               []mountedHandler             // 场景包子路由（前缀 → handler，AP-1：平台不认识场景内容）
	streamStates              streamstate.Provider         // 流式 in-flight 状态（可选）
	logCollector              *LogCollector                // 日志收集器
	workflowStore             *WorkflowStore               // 工作流存储
	teamStore                 *TeamStore                   // 团队数据存储
	budgetCtrl                *engine.BudgetController     // 预算控制器（可选）
	toolCache                 *engine.ToolCache            // 工具缓存（可选）
	toolMetrics               *engine.ToolMetricsCollector // 工具指标（可选）
	toolPerms                 *engine.ToolPermissions      // 工具权限（可选）
	checkpointMgr             *engine.CheckpointManager    // 检查点管理器（可选）
	subagentRegistry          *engine.SubAgentRegistry     // 子 Agent 派生运行注册表（可选，观测/续接）
	cfgTxMgr                  *config.TransactionManager   // v0.4.0 F9 配置事务热加载（可选）
	cronParseProvider         hexagon.Provider             // D2.1 Layer 2 cron parse LLM provider
	cronParseModel            string                       // D2.1 cron parse 模型名（建议 haiku/mini 类快模型）
	version                   string                       // 版本号
	// 自动化权限治理（可选，main.go 经 SetAutonomy 注入）
	autonomyHook      *engine.PermissionHook  // 权限闸引用：Profile 热更新 + 当前策略
	autonomyDecisions *autonomy.DecisionStore // 权限决策审计日志（持久化）
	autonomyGrants    *autonomy.GrantStore    // 任务级授权存储
	autonomyCfgPath   string                  // Profile 持久化目标配置文件（空 = 默认）
	// sandboxPolicyRuntime 以完整策略候选串行提交网络与只读路径，避免运行态半更新。
	sandboxPolicyRuntime   SandboxPolicyRuntime
	server                 *http.Server
	statsMu                sync.Mutex
	statsCache             statsResponse
	statsJSON              []byte
	statsCacheAt           time.Time
	ollamaBaseURL          string
	onOllamaModelInstalled func(context.Context, string)
	serviceLifecycleCtx    context.Context
}

// SandboxPolicy 是一次原子发布的完整沙箱运行策略。
type SandboxPolicy struct {
	NetworkEnabled bool
	ReadablePaths  []string
}

// SandboxPolicyCandidate 表示已经完成构建和验证、但尚未发布的运行时策略。
// Commit 与 Discard 互斥且幂等；Commit 不执行任何可能失败的工作。
type SandboxPolicyCandidate struct {
	state *sandboxPolicyCandidateState
}

type sandboxPolicyCandidateState struct {
	once    sync.Once
	commit  func()
	discard func()
}

// NewSandboxPolicyCandidate 创建只允许完成一次的策略候选。
func NewSandboxPolicyCandidate(commit, discard func()) SandboxPolicyCandidate {
	if commit == nil || discard == nil {
		return SandboxPolicyCandidate{}
	}
	return SandboxPolicyCandidate{state: &sandboxPolicyCandidateState{
		commit:  commit,
		discard: discard,
	}}
}

// Commit 原子发布候选；候选已经完成后重复调用不会产生效果。
func (c SandboxPolicyCandidate) Commit() {
	if c.state == nil {
		return
	}
	c.state.once.Do(c.state.commit)
}

// Discard 放弃候选并释放其写事务；候选已经完成后重复调用不会产生效果。
func (c SandboxPolicyCandidate) Discard() {
	if c.state == nil {
		return
	}
	c.state.once.Do(c.state.discard)
}

func (c SandboxPolicyCandidate) valid() bool {
	return c.state != nil
}

// SandboxPolicyRuntime 提供完整策略的候选事务与单代际快照。
type SandboxPolicyRuntime struct {
	Prepare  func(context.Context, SandboxPolicy) (SandboxPolicyCandidate, error)
	Snapshot func() SandboxPolicy
}

// AgentResourceDetach is the staged half of Agent resource deletion. Commit is
// invoked only after router/store removal succeeds; Rollback compensates the
// staged resources when durable deletion fails. Both callbacks must be
// idempotent.
type AgentResourceDetach struct {
	Commit   func()
	Rollback func(context.Context) error
}

// AgentResourceCleaner stages cleanup of resources owned by an Agent before
// the Agent itself is durably removed.
type AgentResourceCleaner interface {
	DetachAgentResources(
		ctx context.Context,
		agent router.AgentConfig,
	) (AgentResourceDetach, error)
}

// NewServer 创建 API 服务器
//
// gw 可为 nil，此时跳过安全检查（仅限开发模式）。
// store 可为 nil，此时会话/搜索/分支 API 不可用。
func NewServer(cfg *config.Config, eng engine.Engine, gw gateway.Gateway, store storage.Store) *Server {
	collector := NewLogCollector(5000)
	// 挂载日志文件持久化 (JSONL + 轮转)
	sink, err := NewLogFileSink(LogFileSinkConfig{})
	if err != nil {
		logger.Error("[warn] 日志文件持久化初始化失败", "error", err)
	} else {
		AttachToCollector(collector, sink)
		logger.Info("[info] 日志文件", "path", sink.Path())
	}

	return &Server{
		cfg:                cfg,
		engine:             eng,
		gateway:            gw,
		store:              store,
		logCollector:       collector,
		workflowStore:      NewWorkflowStore(),
		teamStore:          NewTeamStore(defaultDataDir()),
		ollamaBaseURL:      defaultOllamaBaseURL,
		credentialResolver: newInMemoryCredentialResolver(),
		attachmentStaging:  newAttachmentStagingStore(),
	}
}

// SetSessionDeletedHook 注册会话删除后的进程内清理回调（如 PermissionHub
// 的 ClearSession）。durable 撤销由 Store.DeleteSession 事务内完成，hook 只
// 负责清理进程内 pending/remembered 状态；hook 缺失或失败只记日志，不改变
// 会话删除结果。
func (s *Server) SetSessionDeletedHook(fn func(sessionID string)) {
	s.sessionDeletedHook = fn
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hexclaw"
	}
	return filepath.Join(home, ".hexclaw")
}

// SetWebSocketHandler 设置 WebSocket Handler
//
// 挂载到 /ws 路径，供 Web UI 使用。
func (s *Server) SetWebSocketHandler(h http.Handler) {
	s.wsHandler = h
}

// SetSidecarCapabilityToken installs the per-start Desktop capability. When
// configured, anonymous loopback access is disabled and HTTP/WebSocket clients
// must present this value as a Bearer token. The token is never persisted.
func (s *Server) SetSidecarCapabilityToken(token string) {
	s.sidecarCapabilityToken = strings.TrimSpace(token)
}

// SetStreamStateProvider 设置流式 in-flight 状态提供器。
func (s *Server) SetStreamStateProvider(p streamstate.Provider) {
	s.streamStates = p
}

// SetKnowledgeBase 设置知识库管理器
//
// 设置后启用知识库 API（文档上传/列表/删除）。
func (s *Server) SetKnowledgeBase(kb *knowledge.Manager) {
	s.kb = kb
}

// SetWebhookManager 设置 Webhook 管理器
//
// 设置后启用 Webhook 接收端点和管理 API。
func (s *Server) SetWebhookManager(mgr *webhook.Manager) {
	s.webhookMgr = mgr
}

// SetCronScheduler 设置 Cron 调度器
//
// 设置后启用定时任务管理 API。
func (s *Server) SetCronScheduler(scheduler *cron.Scheduler) {
	s.scheduler = scheduler
}

// SetConnectorStore 设置数据连接器存储（GitHub/Notion 只读，token 加密）。
//
// 设置后启用 /api/v1/connectors CRUD + test + resources。
func (s *Server) SetConnectorStore(store *connector.Store) {
	s.connectorStore = store
}

// SetPromptStore 设置 Prompt 库存储（§11.8）。设置后启用 /api/v1/prompts CRUD。
func (s *Server) SetPromptStore(ps *library.PromptStore) {
	s.promptStore = ps
}

// SetCronParser 注入 Layer 2 cron 自然语言 → JSON 解析所需的 LLM provider + model（D2.1）。
//
// 该 provider 配合 ResponseFormat=json_object + Tools=nil + Metadata.cron_context=true
// 强制走"纯文本/JSON"路径，从协议层根除 tool_use_id 链路 400 bug。
//
// 入参可为 nil（cron parse 端点会返 needs_clarification 让前端 fallback 到 Layer 3）。
func (s *Server) SetCronParser(provider hexagon.Provider, model string) {
	s.cronParseProvider = provider
	s.cronParseModel = model
}

// SetVectorMemory 设置向量语义记忆
func (s *Server) SetVectorMemory(vm *memory.VectorMemory) {
	s.vectorMem = vm
}

// SetFileMemory 设置文件记忆系统
//
// 设置后启用记忆管理 API。
func (s *Server) SetFileMemory(fm *memory.FileMemory) {
	s.fileMem = fm
}

// SetMCPManager 设置 MCP 管理器
//
// 设置后启用 MCP 工具列表 API。
func (s *Server) SetMCPManager(mgr *hexmcp.Manager) {
	s.mcpMgr = mgr
}

// SetCfgWriter 设置配置文件写入器（MCP 动态添加持久化用）
func (s *Server) SetCfgWriter(w *config.Writer) {
	s.cfgWriter = w
}

// SetSemanticRuntimeInvalidator installs the fail-closed boundary used by
// config hot reloads. It has no UI surface and is intentionally one-way.
func (s *Server) SetSemanticRuntimeInvalidator(invalidate func(context.Context) error) {
	s.invalidateSemanticRuntime = invalidate
}

// SetSemanticRuntimeReloader installs the provider hot-reload boundary. The
// callback receives the fully merged next LLM config before s.cfg is exposed;
// it must return only after the old generation is drained and the successor is
// ready for Catalog/Apply calls.
func (s *Server) SetSemanticRuntimeReloader(
	reload func(context.Context, config.LLMConfig) error,
) {
	s.reloadSemanticRuntime = reload
}

// SetCapabilityService 设置模型 tool_call 能力探测服务（A7）。
// 设置后启用 GET /api/v1/llm/capabilities + POST /probe 端点。
func (s *Server) SetCapabilityService(svc *llmrouter.CapabilityService) {
	s.capabilities = svc
}

// SetMarketplace 设置技能市场
//
// 设置后启用技能安装/列表/删除 API。
// 同时初始化 Hub 客户端用于 ClawHub 在线安装（仓库 URL / 分支见配置 skills.hub）。
func (s *Server) SetMarketplace(mp *marketplace.Marketplace) {
	s.mp = mp
	hc := hub.HubConfig{Enabled: true}
	if s.cfg != nil {
		hc.RepoURL = s.cfg.Skills.Hub.RepoURL
		hc.Branch = s.cfg.Skills.Hub.Branch
	}
	s.skillHub = hub.New(hc, mp.Dir())
	// 启用「最近一次成功拉取」磁盘缓存层（离线优先：内存→磁盘缓存→内嵌种子→后台刷新）。
	s.skillHub.SetCacheDir(hub.DefaultCacheDir())
}

// SetAgentRouter 设置多 Agent 路由器
//
// 设置后启用 Agent 路由管理 API。
func (s *Server) SetAgentRouter(r *router.Dispatcher) {
	s.agentRouter = r
}

// SetSubAgentRegistry 注入子 Agent 派生运行注册表（观测/续接）。
func (s *Server) SetSubAgentRegistry(reg *engine.SubAgentRegistry) {
	s.subagentRegistry = reg
}

// handleListSubAgentRuns GET /api/v1/subagents/runs —— 列出最近的子 Agent 派生运行
// （含角色/深度/状态/耗时/父子树），供桌面端可观测面板/审计使用（feature 3）。
func (s *Server) handleListSubAgentRuns(w http.ResponseWriter, r *http.Request) {
	if s.subagentRegistry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []any{}})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.subagentRegistry.List(limit)})
}

// SetAgentStore 设置 Agent/Rule 持久化层
func (s *Server) SetAgentStore(store router.Store) {
	s.agentStore = store
}

// SetAgentMetadataGuard injects scenario-owned validation without teaching the
// platform API any scenario metadata keys or value semantics.
func (s *Server) SetAgentMetadataGuard(guard func(map[string]string) error) {
	s.agentMetadataGuard = guard
}

// SetAgentResourceCleaner wires the lifecycle boundary for Agent-owned
// resources such as scenario-provisioned cron jobs.
func (s *Server) SetAgentResourceCleaner(cleaner AgentResourceCleaner) {
	s.agentResources = cleaner
}

// SetInstanceManager 设置平台实例运行时管理器。
func (s *Server) SetInstanceManager(mgr *instances.Manager) {
	s.instanceMgr = mgr
}

// SetCanvas 设置 Canvas/A2UI 服务
//
// 设置后启用面板管理 API。
func (s *Server) SetCanvas(svc *canvas.Service) {
	s.canvasSvc = svc
}

// SetVoice 设置语音服务
//
// 设置后启用语音转录/合成 API。
func (s *Server) SetVoice(svc *voice.Service) {
	s.voiceSvc = svc
}

// SetImageGen 设置图像生成服务
//
// 设置后启用 /api/v1/images/* 端点。
func (s *Server) SetImageGen(svc *imagegen.Service) {
	s.imagegenSvc = svc
}

// SetVideoGen 设置视频生成服务
//
// 设置后启用 /api/v1/videos/* 端点（含异步轮询）。
func (s *Server) SetVideoGen(svc *videogen.Service) {
	s.videogenSvc = svc
}

// SetRenderService 设置文档渲染服务。
//
// 设置后启用 POST /api/v1/render 端点。Markdown → docx/pdf/epub/odt/rtf/txt/html/md
// 详见 .claude/doc-generation-architecture.md
func (s *Server) SetRenderService(svc *render.Service) {
	s.renderSvc = svc
}

// SetVoiceChat 设置语音对话服务
//
// 设置后启用 /api/v1/voicechat/* 端点。
func (s *Server) SetVoiceChat(svc *voicechat.Service) {
	s.voiceChatSvc = svc
}

// SetGenStore 设置生成内容存储。
//
// 注入后 image/video 生成结果会持久化到磁盘，并通过 /api/v1/files/generated/{path} 提供访问。
func (s *Server) SetGenStore(st *genstore.Store) {
	s.genStore = st
}

// SetGenServicesReloader 注册"LLM 配置变更后重建 image/video/voice chat 生成服务"的回调。
//
// 为什么：gen services 在启动时根据 cfg.LLM.Providers 里的 API Key 构建 provider。
// 用户通过 UI 后补 API Key 后，LLM config 会热更新，但 gen services 仍是旧的（无 provider）。
// 此回调让 handleUpdateLLMConfig 在配置保存 + LLM 引擎热更新后主动触发 gen services 重建。
func (s *Server) SetGenServicesReloader(fn func()) {
	s.reloadGenServices = fn
}

// handleGeneratedFile GET /api/v1/files/generated/{path...}
//
// 流式返回 genStore 中的文件。MIME 由扩展名推断。
func (s *Server) handleGeneratedFile(w http.ResponseWriter, r *http.Request) {
	if s.genStore == nil {
		http.Error(w, "genstore disabled", http.StatusServiceUnavailable)
		return
	}
	rel := r.PathValue("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	f, err := s.genStore.Open(rel)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	// MIME 推断
	ct := "application/octet-stream"
	switch ext := strings.ToLower(filepath.Ext(rel)); ext {
	case ".png":
		ct = "image/png"
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".webp":
		ct = "image/webp"
	case ".gif":
		ct = "image/gif"
	case ".mp4":
		ct = "video/mp4"
	case ".webm":
		ct = "video/webm"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// ?download=1 或 ?download=<filename> → Content-Disposition: attachment 触发浏览器下载
	// 用途：Tauri WKWebView 下 <a download> 和 blob URL 都不可靠，走 HTTP header 最稳
	if dl := r.URL.Query().Get("download"); dl != "" {
		name := dl
		if name == "1" || name == "true" {
			name = stat.Name()
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}

// SetSandboxPolicyRuntime 注入沙箱完整策略的原子运行时边界。
func (s *Server) SetSandboxPolicyRuntime(runtime SandboxPolicyRuntime) {
	s.sandboxPolicyRuntime = runtime
}

// LogCollector 返回日志收集器，供外部模块写入日志
func (s *Server) LogCollector() *LogCollector {
	return s.logCollector
}

// SetVersion 设置版本号
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetDesktop 设置桌面集成服务
//
// 设置后启用桌面通知、剪贴板等 API。
func (s *Server) SetDesktop(svc *desktop.Service) {
	s.desktopSvc = svc
}

// SetBudgetController 设置预算控制器
//
// 设置后启用预算状态 API。
func (s *Server) SetBudgetController(b *engine.BudgetController) {
	s.budgetCtrl = b
}

// SetToolCache 设置工具缓存
//
// 设置后启用工具缓存统计 API。
func (s *Server) SetToolCache(tc *engine.ToolCache) {
	s.toolCache = tc
}

// SetToolMetrics 设置工具指标收集器
//
// 设置后启用工具指标 API。
func (s *Server) SetToolMetrics(tm *engine.ToolMetricsCollector) {
	s.toolMetrics = tm
}

// SetToolPermissions 设置工具权限控制器
//
// 设置后启用工具权限查询 API。
func (s *Server) SetToolPermissions(tp *engine.ToolPermissions) {
	s.toolPerms = tp
}

// SetCheckpointManager 设置检查点管理器
//
// 设置后启用检查点列表 API。
// SetConfigTxManager 注入 v0.4.0 F9 事务热加载 manager。
//
// 设置后 PUT /api/v1/config/llm 走 Begin → Stage → Save → Commit/Rollback 路径，
// flag config.tx.hotload.v1 OFF 时自动降级到原有 ReloadLLMConfig 静态路径。
func (s *Server) SetConfigTxManager(tm *config.TransactionManager) {
	s.cfgTxMgr = tm
}

func (s *Server) SetCheckpointManager(cm *engine.CheckpointManager) {
	s.checkpointMgr = cm
}

// mountedHandler 一个挂在前缀下的子路由（场景包提供）。
type mountedHandler struct {
	prefix string
	h      http.Handler
}

// Mount 把一个子路由 handler 挂到路径前缀下（如 "/api/k12"）。
//
// AP-1：平台 api 层不认识任何场景内容，只做通用挂载；子路由由场景包（scenarios/k12/apihttp）
// 自己提供。须在 routes() 被调用（服务启动）前调用。
func (s *Server) Mount(prefix string, h http.Handler) {
	if prefix == "" || h == nil {
		return
	}
	s.extraMounts = append(s.extraMounts, mountedHandler{prefix: prefix, h: h})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /health", s.handleHealth)

	// API v1
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)
	mux.HandleFunc("POST /api/v1/attachments", s.handleStageAttachment)
	// Native-only internal API. apiAuthMiddleware requires an exact loopback
	// sidecar capability and rejects the general API token for this namespace.
	mux.HandleFunc("POST /api/internal/desktop/credentials/hydrate", s.handleHydrateDesktopCredentials)
	mux.HandleFunc("POST /api/internal/desktop/credentials/dehydrate", s.handleDehydrateDesktopCredentials)
	mux.HandleFunc("POST /api/internal/desktop/provider-credentials/reserve", s.handleReserveProviderCredentialIdentity)

	// 文档渲染 API（markdown → docx/pdf/epub/odt/rtf/txt/html/md）
	if s.renderSvc != nil {
		mux.HandleFunc("POST /api/v1/render", s.handleRender)
	}

	// 知识库 API
	if s.kb != nil {
		mux.HandleFunc("POST /api/v1/knowledge/documents", s.handleAddDocument)
		mux.HandleFunc("GET /api/v1/knowledge/documents", s.handleListDocuments)
		mux.HandleFunc("GET /api/v1/knowledge/documents/{id}", s.handleGetDocument)
		mux.HandleFunc("DELETE /api/v1/knowledge/documents/{id}", s.handleDeleteDocument)
		mux.HandleFunc("POST /api/v1/knowledge/documents/{id}/reindex", s.handleReindexDocument)
		mux.HandleFunc("POST /api/v1/knowledge/search", s.handleSearchKnowledge)
		mux.HandleFunc("GET /api/v1/knowledge/metrics", s.handleKnowledgeRetrievalMetrics)
		mux.HandleFunc("GET /api/v1/knowledge/config", s.handleGetKnowledgeConfig)
		mux.HandleFunc("GET /api/v1/knowledge/embedding-status", s.handleKnowledgeEmbeddingStatus)
		mux.HandleFunc("PUT /api/v1/knowledge/config", s.handlePutKnowledgeConfig)
	} else {
		mux.HandleFunc("GET /api/v1/knowledge/documents", emptyList("documents"))
	}
	if s.semanticIndex != nil {
		mux.HandleFunc("GET /api/v1/knowledge/operations", s.handleKnowledgeOperations)
		mux.HandleFunc("POST /api/v1/knowledge/operations/{operation_id}/ack", s.handleAcknowledgeKnowledgeOperation)
		mux.HandleFunc("POST /api/v1/knowledge/documents/{id}/retry", s.handleRetryKnowledgeDocument)
		mux.HandleFunc("GET /api/v1/knowledge/corpora/{corpus_id}/embedding-policy", s.handleGetKnowledgeEmbeddingPolicy)
		mux.HandleFunc("POST /api/v1/knowledge/corpora/{corpus_id}/embedding-policy:apply", s.handleApplyKnowledgeEmbeddingPolicy)
		mux.HandleFunc("GET /api/v1/knowledge/jobs/{job_id}", s.handleGetKnowledgeJob)
		mux.HandleFunc("POST /api/v1/knowledge/jobs/{job_id}/cancel", s.handleCancelKnowledgeJob)
	}

	// 文档解析（无状态，不依赖知识库）：把上传文档抽取为纯文本供对话注入。
	// PDF 在桌面 WKWebView 前端解析不可靠，下沉到后端（复用 hexagon rag/loader）。
	mux.HandleFunc("POST /api/v1/documents/extract", s.handleExtractDocument)
	// 文档原文件预览/下载：暂存原文件并以 http://localhost 提供（前端 shell open 渲染/下载）。
	mux.HandleFunc("POST /api/v1/documents/preview", s.handleDocumentPreviewUpload)
	mux.HandleFunc("GET /api/v1/documents/preview/{token}", s.handleDocumentPreviewGet)

	// 会话 / 搜索 / 分支 API
	if s.store != nil {
		mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
		mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
		mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
		mux.HandleFunc("PATCH /api/v1/sessions/{id}", s.handleUpdateSession)
		mux.HandleFunc("POST /api/v1/sessions/{id}/suggest-title", s.handleSuggestSessionTitle)
		mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
		mux.HandleFunc("GET /api/v1/sessions/{id}/messages", s.handleListMessages)
		mux.HandleFunc("POST /api/v1/sessions/{id}/messages", s.handleAppendMessage)
		mux.HandleFunc("POST /api/v1/sessions/{id}/messages/batch", s.handleBatchAppendMessages)
		mux.HandleFunc("GET /api/v1/sessions/{id}/branches", s.handleListBranches)
		mux.HandleFunc("POST /api/v1/sessions/{id}/fork", s.handleForkSession)
		mux.HandleFunc("GET /api/v1/messages/search", s.handleSearchMessages)
		mux.HandleFunc("DELETE /api/v1/messages/{id}", s.handleDeleteMessage)
		mux.HandleFunc("PUT /api/v1/messages/{id}/feedback", s.handleUpdateMessageFeedback)
	}

	if s.streamStates != nil {
		mux.HandleFunc("GET /api/v1/streams/active", s.handleListActiveStreams)
		mux.HandleFunc("GET /api/v1/streams/{request_id}", s.handleGetStreamSnapshot)
	}

	// 配置 API
	mux.HandleFunc("GET /api/v1/config/llm", s.handleGetLLMConfig)
	mux.HandleFunc("PUT /api/v1/config/llm", s.handleUpdateLLMConfig)
	mux.HandleFunc("POST /api/v1/config/llm/test", s.handleTestLLMConfig)
	mux.HandleFunc("POST /api/v1/config/llm/models", s.handleFetchProviderModels)
	// 记忆行为配置（BUG-20260703 P2-2：auto_memory / 召回地板 / 主动召回 / 画像蒸馏）
	mux.HandleFunc("GET /api/v1/config/memory", s.handleGetMemoryConfig)
	mux.HandleFunc("PUT /api/v1/config/memory", s.handleUpdateMemoryConfig)

	// A7 模型 tool_call 能力探测
	if s.capabilities != nil {
		mux.HandleFunc("GET /api/v1/llm/capabilities", s.handleListCapabilities)
		mux.HandleFunc("POST /api/v1/llm/capabilities/probe", s.handleProbeCapability)
	}

	// §15 连接中心「测试连接」：无状态验证一组连接凭据（email / IM），凭据不持久化、不落日志。
	mux.HandleFunc("POST /api/v1/connections/test", s.handleConnectionsTest)

	// 默认助理（小蟹）人设(SOUL)：读写 ~/.hexclaw/SOUL.md，空=恢复内置默认；引擎每轮读取，保存即时生效。
	mux.HandleFunc("GET /api/v1/assistant/soul", s.handleGetAssistantSoul)
	mux.HandleFunc("PUT /api/v1/assistant/soul", s.handleUpdateAssistantSoul)

	// §15.1 数据连接器：token 只读接入 GitHub / Notion，token 加密落盘、响应脱敏。
	if s.connectorStore != nil {
		mux.HandleFunc("GET /api/v1/connectors", s.handleListConnectors)
		mux.HandleFunc("POST /api/v1/connectors", s.handleCreateConnector)
		mux.HandleFunc("DELETE /api/v1/connectors/{id}", s.handleDeleteConnector)
		mux.HandleFunc("POST /api/v1/connectors/test", s.handleTestConnector)
		mux.HandleFunc("GET /api/v1/connectors/{id}/resources", s.handleConnectorResources)
	}

	// 角色列表 API
	mux.HandleFunc("GET /api/v1/roles", s.handleListRoles)

	// §11.8 Prompt 库 API（服务端下发，运营增删不发版）
	if s.promptStore != nil {
		mux.HandleFunc("GET /api/v1/prompts", s.handleListPrompts)
		mux.HandleFunc("GET /api/v1/prompts/all", s.handleListAllPrompts)
		mux.HandleFunc("POST /api/v1/prompts", s.handleUpsertPrompt)
		mux.HandleFunc("DELETE /api/v1/prompts/{id}", s.handleDeletePrompt)
	}
	// 砍薄版（§5）：/api/v1/memories（旧记忆薄版 CRUD）已移除，长期记忆统一走 /api/v1/memory（文件记忆）。

	// Webhook API
	if s.webhookMgr != nil {
		mux.HandleFunc("POST /api/v1/webhooks/{name}", s.webhookMgr.Handler())
		mux.HandleFunc("GET /api/v1/webhooks", s.handleListWebhooks)
		mux.HandleFunc("POST /api/v1/webhooks", s.handleRegisterWebhook)
		mux.HandleFunc("PATCH /api/v1/webhooks/{name}", s.handleUpdateWebhook)
		mux.HandleFunc("DELETE /api/v1/webhooks/{name}", s.handleDeleteWebhook)
	}

	// 自动化权限治理 API（Profile / 预检 / 总览 / 决策日志 / 任务级授权）
	mux.HandleFunc("GET /api/v1/autonomy/profile", s.handleGetAutonomyProfile)
	mux.HandleFunc("PUT /api/v1/autonomy/profile", s.handleUpdateAutonomyProfile)
	mux.HandleFunc("POST /api/v1/autonomy/preflight", s.handleAutonomyPreflight)
	mux.HandleFunc("GET /api/v1/autonomy/summary", s.handleAutonomySummary)
	mux.HandleFunc("GET /api/v1/autonomy/decisions", s.handleListAutonomyDecisions)
	mux.HandleFunc("GET /api/v1/autonomy/grants", s.handleListAutonomyGrants)
	mux.HandleFunc("POST /api/v1/autonomy/grants", s.handleCreateAutonomyGrant)
	mux.HandleFunc("DELETE /api/v1/autonomy/grants/{id}", s.handleRevokeAutonomyGrant)

	// Cron API（v0.4.x：统一为单 endpoint 7-action 入口，决策 B 完全替换）
	if s.scheduler != nil {
		// 主入口：D1.2 统一 endpoint（create/update/list/pause/resume/remove/run）
		mux.HandleFunc("POST /api/v1/cronjob", s.handleCronjobUnified)
		// Layer 2 LLM JSON 解析（cron_context 守卫强制 tools=nil 绕开 tool_use_id 400 链路）
		mux.HandleFunc("POST /api/v1/cron/parse", s.handleCronParse)
		// SSE 创建仍保留（流式编译进度需要 EventSource，无法塞进 unified JSON）
		mux.HandleFunc("POST /api/v1/cron/jobs/stream", s.handleAddCronJobSSE)
		// 历史查询保留（与 unified action 概念正交）
		mux.HandleFunc("GET /api/v1/cron/jobs/{id}/history", s.handleCronJobHistory)
	}

	// 文件记忆 API
	if s.fileMem != nil {
		mux.HandleFunc("GET /api/v1/memory", s.handleGetMemory)
		mux.HandleFunc("POST /api/v1/memory", s.handleSaveMemory)
		mux.HandleFunc("PUT /api/v1/memory", s.handleUpdateMemory)
		mux.HandleFunc("PUT /api/v1/memory/{id}", s.handleUpdateMemory)
		mux.HandleFunc("POST /api/v1/memory/{id}/archive", s.handleArchiveMemoryItem)
		mux.HandleFunc("POST /api/v1/memory/{id}/restore", s.handleRestoreMemoryItem)
		mux.HandleFunc("POST /api/v1/memory/{id}/pin", s.handlePinMemoryItem)
		mux.HandleFunc("POST /api/v1/memory/{id}/unpin", s.handleUnpinMemoryItem)
		mux.HandleFunc("DELETE /api/v1/memory/{id}", s.handleDeleteMemoryItem)
		mux.HandleFunc("DELETE /api/v1/memory", s.handleDeleteMemory)
		mux.HandleFunc("GET /api/v1/memory/search", s.handleSearchMemory)
	} else {
		mux.HandleFunc("GET /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "summary": "", "capacity": map[string]int{"used": 0, "max": 0}})
		})
		mux.HandleFunc("DELETE /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"message": "记忆模块未启用"})
		})
	}

	// MCP API
	if s.mcpMgr != nil {
		mux.HandleFunc("GET /api/v1/mcp/tools", s.handleListMCPTools)
		mux.HandleFunc("GET /api/v1/mcp/servers", s.handleListMCPServers)
		mux.HandleFunc("POST /api/v1/mcp/servers", s.handleAddMCPServer)
		mux.HandleFunc("DELETE /api/v1/mcp/servers/{name}", s.handleRemoveMCPServer)
		mux.HandleFunc("POST /api/v1/mcp/servers/{name}/restart", s.handleRestartMCPServer) // M3-20260710
		mux.HandleFunc("POST /api/v1/mcp/tools/call", s.handleCallMCPTool)
		mux.HandleFunc("GET /api/v1/mcp/status", s.handleMCPStatus)
	} else {
		mux.HandleFunc("GET /api/v1/mcp/servers", emptyList("servers"))
		mux.HandleFunc("GET /api/v1/mcp/tools", emptyList("tools"))
		mux.HandleFunc("GET /api/v1/mcp/status", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"servers": []any{}, "total": 0})
		})
		mux.HandleFunc("POST /api/v1/mcp/tools/call", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"error": "MCP 模块未启用"})
		})
	}

	// 技能市场 API
	if s.mp != nil {
		mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)
		mux.HandleFunc("GET /api/v1/skills/{name}/content", s.handleSkillContent)
		mux.HandleFunc("PUT /api/v1/skills/{name}/status", s.handleSkillStatus)
		mux.HandleFunc("POST /api/v1/skills/install", s.handleInstallSkill)
		mux.HandleFunc("POST /api/v1/skills/generate", s.handleGenerateSkill)
		mux.HandleFunc("DELETE /api/v1/skills/{name}", s.handleUninstallSkill)
	}

	// 多 Agent 路由 API
	if s.agentRouter != nil {
		mux.HandleFunc("GET /api/v1/agents", s.handleListAgents)
		mux.HandleFunc("POST /api/v1/agents", s.handleRegisterAgent)
		mux.HandleFunc("PUT /api/v1/agents/{name}", s.handleUpdateAgent)
		mux.HandleFunc("DELETE /api/v1/agents/{name}", s.handleUnregisterAgent)
		mux.HandleFunc("POST /api/v1/agents/default", s.handleSetDefaultAgent)
		mux.HandleFunc("GET /api/v1/agents/rules", s.handleListRules)
		mux.HandleFunc("POST /api/v1/agents/rules", s.handleAddRule)
		mux.HandleFunc("POST /api/v1/agents/rules/test", s.handleTestRoute)
		mux.HandleFunc("DELETE /api/v1/agents/rules/{id}", s.handleDeleteRule)
	} else {
		mux.HandleFunc("GET /api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"agents":  []any{},
				"rules":   []any{},
				"total":   0,
				"default": "",
			})
		})
		mux.HandleFunc("GET /api/v1/agents/rules", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"rules": []any{},
				"total": 0,
			})
		})
	}

	if s.instanceMgr != nil {
		// 连接中心脱敏只读列表（一处存）：永不下发凭据。
		mux.HandleFunc("GET /api/v1/connections", s.handleListConnections)
		mux.HandleFunc("GET /api/v1/platforms/instances", s.handleListInstances)
		mux.HandleFunc("GET /api/v1/platforms/instances/health", s.handleListInstanceHealth)
		mux.HandleFunc("POST /api/v1/platforms/instances", s.handleUpsertInstance)
		mux.HandleFunc("PUT /api/v1/platforms/instances/by-id/{id}", s.handleUpdateInstanceByID)
		mux.HandleFunc("DELETE /api/v1/platforms/instances/by-id/{id}", s.handleDeleteInstanceByID)
		mux.HandleFunc("POST /api/v1/platforms/instances/by-id/{id}/test", s.handleTestInstanceByID)
		mux.HandleFunc("POST /api/v1/platforms/instances/by-id/{id}/send-test", s.handleSendTestInstanceByID)
		mux.HandleFunc("PUT /api/v1/platforms/instances/{name}", s.handleUpsertInstance)
		mux.HandleFunc("DELETE /api/v1/platforms/instances/{name}", s.handleDeleteInstance)
		mux.HandleFunc("GET /api/v1/platforms/instances/{name}/health", s.handleGetInstanceHealth)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/test", s.handleTestInstance)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/start", s.handleStartInstance)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/stop", s.handleStopInstance)
		mux.HandleFunc("POST /api/v1/im/channels/{provider}/test", s.handleTestChannelConfig)
		mux.HandleFunc("GET /api/v1/channels/wecom/guide", s.handleWecomGuide)
		mux.HandleFunc("GET /api/v1/platforms/hooks/{provider}/{name}", s.handlePlatformHook)
		mux.HandleFunc("POST /api/v1/platforms/hooks/{provider}/{name}", s.handlePlatformHook)
	}

	// Canvas/A2UI API
	if s.canvasSvc != nil {
		mux.HandleFunc("GET /api/v1/canvas/panels", s.handleListPanels)
		mux.HandleFunc("GET /api/v1/canvas/panels/{id}", s.handleGetPanel)
		mux.HandleFunc("POST /api/v1/canvas/events", s.handleCanvasEvent)
	}

	// Canvas Workflow API（始终启用，内存存储）
	mux.HandleFunc("GET /api/v1/canvas/workflows", s.handleListWorkflows)
	mux.HandleFunc("POST /api/v1/canvas/workflows", s.handleSaveWorkflow)
	mux.HandleFunc("DELETE /api/v1/canvas/workflows/{id}", s.handleDeleteWorkflow)
	mux.HandleFunc("POST /api/v1/canvas/workflows/{id}/run", s.handleRunWorkflow)
	mux.HandleFunc("GET /api/v1/canvas/runs/{id}", s.handleGetWorkflowRun)
	mux.HandleFunc("POST /api/v1/canvas/runs/{id}/resume", s.handleResumeWorkflowRun)
	mux.HandleFunc("GET /api/v1/subagents/runs", s.handleListSubAgentRuns)

	// 语音 API
	if s.voiceSvc != nil {
		mux.HandleFunc("GET /api/v1/voice/status", s.handleVoiceStatus)
		mux.HandleFunc("POST /api/v1/voice/transcribe", s.handleVoiceTranscribe)
		mux.HandleFunc("POST /api/v1/voice/synthesize", s.handleVoiceSynthesize)
	}

	// 图像生成 API（status 始终注册，便于前端探测；generate 仅在配置后可用）
	mux.HandleFunc("GET /api/v1/images/status", s.handleImageGenStatus)
	mux.HandleFunc("POST /api/v1/images/generate", s.handleImageGenGenerate)

	// 视频生成 API（异步两步：submit + poll）
	mux.HandleFunc("GET /api/v1/videos/status", s.handleVideoGenStatus)
	mux.HandleFunc("POST /api/v1/videos/generate", s.handleVideoGenSubmit)
	mux.HandleFunc("GET /api/v1/videos/tasks/{id}", s.handleVideoGenPoll)

	// 语音对话 API（audio-to-audio，gpt-4o-audio）
	mux.HandleFunc("GET /api/v1/voicechat/status", s.handleVoiceChatStatus)
	mux.HandleFunc("POST /api/v1/voicechat/chat", s.handleVoiceChat)

	// 生成内容文件服务（图像 / 视频 / 语音持久化产物）
	mux.HandleFunc("GET /api/v1/files/generated/{path...}", s.handleGeneratedFile)

	// 日志 API（始终启用）
	mux.HandleFunc("GET /api/v1/logs", s.handleGetLogs)
	mux.HandleFunc("GET /api/v1/logs/stats", s.handleGetLogStats)
	mux.HandleFunc("GET /api/v1/logs/stream", s.handleLogStream)

	// 系统 API（始终启用）
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)
	mux.HandleFunc("GET /api/v1/config", s.handleGetFullConfig)
	mux.HandleFunc("PUT /api/v1/config", s.handleUpdateFullConfig)
	mux.HandleFunc("GET /api/v1/models", s.handleListModels)
	mux.HandleFunc("GET /api/v1/ollama/status", s.handleOllamaStatus)
	mux.HandleFunc("POST /api/v1/ollama/pull", s.handleOllamaPull)
	mux.HandleFunc("GET /api/v1/ollama/running", s.handleOllamaRunning)
	mux.HandleFunc("POST /api/v1/ollama/load", s.handleOllamaLoad)
	mux.HandleFunc("POST /api/v1/ollama/unload", s.handleOllamaUnload)
	mux.HandleFunc("DELETE /api/v1/ollama/models/{name}", s.handleOllamaDelete)
	mux.HandleFunc("POST /api/v1/ollama/restart", s.handleOllamaRestart)

	// ClawHub 搜索（Skill 市场）
	mux.HandleFunc("GET /api/v1/clawhub/search", s.handleClawHubSearch)
	// ClawHub 技能「安装前预览」：不落盘返回 SKILL.md 原文
	mux.HandleFunc("GET /api/v1/clawhub/skills/{name}/content", s.handleClawHubSkillContent)

	// Team API（共享 Agent + 团队成员）
	mux.HandleFunc("GET /api/v1/team/agents", s.handleListSharedAgents)
	mux.HandleFunc("POST /api/v1/team/agents", s.handleShareAgent)
	mux.HandleFunc("DELETE /api/v1/team/agents/{id}", s.handleDeleteSharedAgent)
	mux.HandleFunc("GET /api/v1/team/members", s.handleListTeamMembers)
	mux.HandleFunc("POST /api/v1/team/members", s.handleInviteTeamMember)
	mux.HandleFunc("DELETE /api/v1/team/members/{id}", s.handleRemoveTeamMember)

	// 预算状态 API
	if s.budgetCtrl != nil {
		mux.HandleFunc("GET /api/v1/budget/status", s.handleBudgetStatus)
	}

	// 工具缓存统计 API
	if s.toolCache != nil {
		mux.HandleFunc("GET /api/v1/tools/cache/stats", s.handleToolCacheStats)
	}

	// 工具指标 API
	if s.toolMetrics != nil {
		mux.HandleFunc("GET /api/v1/tools/metrics", s.handleToolMetrics)
	}

	// 工具权限 API
	if s.toolPerms != nil {
		mux.HandleFunc("GET /api/v1/tools/permissions", s.handleToolPermissions)
	}

	// 检查点列表 API
	if s.checkpointMgr != nil {
		mux.HandleFunc("GET /api/v1/sessions/{id}/checkpoints", s.handleListCheckpoints)
	}

	// 工具审批仅由 WebAdapter ↔ 持久化 PermissionHub 契约处理；
	// 此处不挂载并行的 REST 审批路由。

	// 桌面集成 API
	if s.desktopSvc != nil {
		s.desktopSvc.RegisterRoutes(mux)
	}

	// 场景包子路由（AP-1 通用挂载；如 K12 挂在 /api/k12/）。
	for _, m := range s.extraMounts {
		mux.Handle(m.prefix+"/", http.StripPrefix(m.prefix, m.h))
	}

	// WebSocket（Web UI）
	if s.wsHandler != nil {
		mux.Handle("/ws", s.wsHandler)
	}

	// 顶层 panic 兜底 → 认证 → CORS → 路由（bug 2026-06-22 P0-2：此前无 recover 中间件，
	// handler panic 会中断响应且不返回结构化错误）
	return recoverMiddleware(s.apiAuthMiddleware(corsMiddleware(mux)))
}

// recoverMiddleware 捕获下游 handler / 中间件的 panic，返回 500 JSON 并保持进程存活。
// 流式 handler 若已写过部分响应，这里的 writeJSON 会触发一次无害的 superfluous WriteHeader。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("[api] panic recovered",
					"path", r.URL.Path, "method", r.Method,
					"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "内部错误"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Start 启动 HTTP 服务器
//
// 注册路由并开始监听。此方法会阻塞直到服务器停止。
// 使用 Stop() 方法触发优雅关闭。
// Start 启动 HTTP 服务。
//
// 行为顺序：bind 端口 → 调用 onReady → 进入 Serve 循环。
// 任何 bind 错误同步返回，便于调用方 fail-fast 并输出真实错误。
// onReady 在端口已监听后触发，用于"已就绪"这种只应在真实就绪后展示的日志。
func (s *Server) Start(ctx context.Context, onReady func()) error {
	s.server = s.buildHTTPServer(ctx)
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.server.Addr, err)
	}
	if onReady != nil {
		onReady()
	}
	return s.server.Serve(listener)
}

func (s *Server) buildHTTPServer(ctx context.Context) *http.Server {
	if ctx == nil {
		ctx = context.Background()
	}
	// Long-running operations may intentionally detach from one browser
	// request, but must still be owned by the serving process. This is assigned
	// during construction, before Serve exposes any handler concurrently.
	s.serviceLifecycleCtx = ctx
	handler := s.routes()
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
}

// emptyList 返回空列表响应（用于未启用模块的 fallback）
func emptyList(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{key: []any{}, "total": 0})
	}
}

// Stop 优雅关闭服务器
//
// 使用调用方传入的 context 控制超时，避免双重超时。
func (s *Server) Stop(ctx context.Context) error {
	return s.StopWithDrain(ctx, nil)
}

// StopWithDrain closes every HTTP listener before invoking drain, then waits
// for in-flight requests to become idle. The hook is intended for cancelling
// process-owned workers and detached streaming operations: no new request can
// enter once it runs, while existing requests still receive graceful shutdown.
func (s *Server) StopWithDrain(ctx context.Context, drain func()) error {
	var drainOnce sync.Once
	runDrain := func() {
		if drain != nil {
			drainOnce.Do(drain)
		}
	}
	if s.server == nil {
		runDrain()
		return s.attachmentStaging.Close()
	}
	if drain != nil {
		s.server.RegisterOnShutdown(runDrain)
	}
	err := s.server.Shutdown(ctx)
	// RegisterOnShutdown callbacks run asynchronously. Ensure the caller never
	// observes StopWithDrain returning before its runtime cancellation ran.
	runDrain()
	return errors.Join(err, s.attachmentStaging.Close())
}

// handleHealth 健康检查端点
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.engine != nil {
		if err := s.engine.Health(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unhealthy",
				"error":  err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

// ChatRequest 聊天请求
type ChatRequest struct {
	Message     string               `json:"message"`               // 用户消息内容
	SessionID   string               `json:"session_id,omitempty"`  // 会话 ID（可选，空则创建新会话）
	UserID      string               `json:"user_id,omitempty"`     // 用户 ID（可选）
	Role        string               `json:"role,omitempty"`        // Agent 角色（可选：assistant/researcher/writer/coder/translator/analyst）
	Provider    string               `json:"provider,omitempty"`    // 显式指定 Provider（可选）
	Model       string               `json:"model,omitempty"`       // 显式指定模型（可选）
	Platform    string               `json:"platform,omitempty"`    // 来源平台（可选：api/desktop，未传时自动推断）
	Attachments []adapter.Attachment `json:"attachments,omitempty"` // 图片附件列表（可选）
	Metadata    map[string]string    `json:"metadata,omitempty"`    // 请求级元数据（如 thinking/memory）
	RequestID   string               `json:"request_id,omitempty"`  // 客户端请求 ID（用于幂等/流式恢复关联）
	Temperature *float64             `json:"temperature,omitempty"` // 本次请求温度；nil=跟随 Agent/模型默认，0=确定性
	MaxTokens   *int                 `json:"max_tokens,omitempty"`  // 本次请求最大输出 token；nil=跟随 Agent/模型默认
}

// ChatResponse 聊天回复
type ChatResponse struct {
	Reply          string                         `json:"reply"`                     // 回复内容（legacy fallback）
	MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"` // canonical Markdown/LaTeX
	RenderManifest *messagecontent.RenderManifest `json:"render_manifest,omitempty"` // projection receipt when supplied by owner surface
	SessionID      string                         `json:"session_id"`                // 会话 ID
	Metadata       map[string]string              `json:"metadata,omitempty"`        // 元数据
	Usage          *adapter.Usage                 `json:"usage,omitempty"`           // Token 使用统计
	ToolCalls      []adapter.ToolCall             `json:"tool_calls,omitempty"`      // 工具调用记录
	Blocks         []adapter.Block                `json:"blocks,omitempty"`          // 有序内容块（多步交错按序渲染）
	// U9：结构化 RAG/记忆命中（非空时前端渲染「知识库命中」「记忆命中」标签+详情）。
	KnowledgeHits       []adapter.KnowledgeHit          `json:"knowledge_hits,omitempty"`
	MemoryHits          []adapter.MemoryHit             `json:"memory_hits,omitempty"`
	AssistantMessageID  string                          `json:"assistant_message_id,omitempty"`
	BackendMessageID    string                          `json:"backend_message_id,omitempty"`
	MessageID           string                          `json:"message_id,omitempty"`
	LastSequence        uint64                          `json:"last_sequence,omitempty"`
	ReasoningDisclosure adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
	RuntimeEvents       []adapter.SequencedRuntimeEvent `json:"runtime_events,omitempty"`
}

// handleChat 同步聊天端点
//
// 请求体: {"message": "你好", "session_id": "optional", "user_id": "optional"}
// 响应: {"reply": "你好！有什么可以帮助你的？", "session_id": "sess-xxx"}
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "引擎未就绪",
		})
		return
	}

	// 解析请求（限制请求体大小为 20MB，支持图片附件）
	const maxRequestBodySize = 20 << 20 // 20MB
	var req ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodySize)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}
	// Resolve opaque staging IDs under the authenticated principal before any
	// attachment metadata reaches validation or the engine. The request cannot
	// provide filename, MIME, digest, or bytes for an ID reference.
	principal := httpPrincipalFromRequest(r)
	resolvedAttachments, err := s.ResolveStagedAttachments(r.Context(), principal.userID, req.Attachments)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "attachment is unavailable"})
		return
	}
	req.Attachments = resolvedAttachments

	if !adapter.HasMessageInput(req.Message, req.Attachments) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "message 不能为空",
		})
		return
	}
	if err := adapter.ValidateAttachments(req.Attachments); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	platform, err := resolveChatPlatform(req, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	// Validate an explicit provider before constructing/persisting a chat
	// message. Invalid client input is 400 and must not leave a partial session.
	if validator, ok := s.engine.(interface{ ValidateProvider(string) error }); ok {
		if err := validator.ValidateProvider(req.Provider); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": upstreamerr.PublicMessage(err, "error"),
			})
			return
		}
	}
	if err := validateRequestedCompletionModel(s.activeLLMConfig(), req.Provider, req.Model); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// HTTP identity is derived exclusively by apiAuthMiddleware. Client body
	// platform/user_id fields remain decode-compatible but carry no authority.
	userID := principal.userID

	msg := &adapter.Message{
		ID:          "msg-" + idgen.ShortID(),
		Platform:    platform,
		UserID:      userID,
		UserName:    userID,
		SessionID:   req.SessionID,
		Content:     req.Message,
		Attachments: req.Attachments,
		Timestamp:   time.Now(),
		Metadata:    make(map[string]string),
	}

	for k, v := range req.Metadata {
		msg.Metadata[k] = v
	}
	// GO-3：外部聊天入口是信任边界——剥除只能由受信内部派发器盖章的保留键，
	// 否则客户端可伪造 source=cron + cron_job_id 盗用他人任务的授权（提权）。
	engine.StripReservedDispatchMetadata(msg.Metadata)
	metadataModel := strings.TrimSpace(msg.Metadata["model"])
	if metadataModel == "" {
		metadataModel = strings.TrimSpace(msg.Metadata["agent_model"])
	}
	if err := validateRequestedCompletionModel(
		s.activeLLMConfig(),
		msg.Metadata["provider"],
		metadataModel,
	); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := adapter.ApplyRequestSamplingOverrides(msg.Metadata, req.Temperature, req.MaxTokens); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.RequestID != "" {
		msg.Metadata["request_id"] = req.RequestID
	}

	// 如果指定了角色，通过元数据传递给引擎；显式字段优先级高于 metadata。
	if req.Role != "" {
		msg.Metadata["role"] = req.Role
	}
	if req.Provider != "" {
		msg.Metadata["provider"] = req.Provider
	}
	if req.Model != "" {
		msg.Metadata["model"] = req.Model
	}

	// 请求级结构化日志
	logger := trace.NewRequest(userID, "").With("source", "chat", "provider", req.Provider, "model", req.Model)
	ctx := trace.WithLogger(r.Context(), logger)
	logger.Info("← 收到消息", "content_len", len([]rune(req.Message)), "platform", string(msg.Platform))

	// 安全网关检查
	if s.gateway != nil {
		if err := s.gateway.Check(ctx, msg); err != nil {
			if gwErr, ok := err.(*gateway.GatewayError); ok {
				trace.L(ctx).Warn("安全检查拒绝", "layer", gwErr.Layer, "code", gwErr.Code)
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": gwErr.Message,
					"code":  gwErr.Code,
					"layer": gwErr.Layer,
				})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "安全检查异常",
				})
			}
			return
		}
	}

	// 调用引擎处理（优先走流式路径，避免 thinking 模型阻塞工具探测）

	type streamEngine interface {
		ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error)
	}

	// BUG-20260523-v2 架构修复：检测 SSE 流式请求
	// 当客户端声明 Accept: text/event-stream 时，HTTP 层与 ProcessStream 同步流式输出，
	// 不再把所有 chunk 攒成完整 JSON 才返回。
	// 避免前端因"LLM 推理时长不可预测"被迫设固定 HTTP 总时长 timeout。
	wantsSSE := strings.Contains(r.Header.Get("Accept"), "text/event-stream")

	if wantsSSE {
		se, ok := s.engine.(streamEngine)
		if !ok {
			trace.L(ctx).Error("引擎不支持流式但收到 SSE 请求")
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "engine does not support streaming",
			})
			return
		}
		s.handleChatSSE(ctx, w, msg, se, start)
		return
	}

	var reply *adapter.Reply
	if se, ok := s.engine.(streamEngine); ok {
		chunks, err := se.ProcessStream(ctx, msg)
		if err != nil {
			trace.L(ctx).Error("处理失败", "err", err)
			writeJSON(w, chatErrorStatus(req.Provider, err), map[string]string{
				"error": upstreamerr.PublicMessage(err, "error"),
			})
			return
		}
		// 消费流式 channel，收集完整回复
		var content strings.Builder
		var metadata map[string]string
		var usage *adapter.Usage
		var toolCalls []adapter.ToolCall
		var blocks []adapter.Block
		var knowledgeHits []adapter.KnowledgeHit
		var memoryHits []adapter.MemoryHit
		var assistantMessageID string
		var backendMessageID string
		var messageID string
		var lastSequence uint64
		var reasoningDisclosure adapter.ReasoningDisclosure
		var runtimeEvents []adapter.SequencedRuntimeEvent
		for chunk := range chunks {
			if chunk.Error != nil {
				trace.L(ctx).Error("处理失败", "err", chunk.Error)
				writeJSON(w, chatErrorStatus(req.Provider, chunk.Error), map[string]string{
					"error": upstreamerr.PublicMessage(chunk.Error, "error"),
				})
				return
			}
			content.WriteString(chunk.Content)
			assistantMessageID = chunk.AssistantMessageID
			backendMessageID = chunk.BackendMessageID
			messageID = chunk.MessageID
			lastSequence = chunk.Sequence
			reasoningDisclosure = chunk.ReasoningDisclosure
			if chunk.RuntimeEvent != nil {
				runtimeEvents = append(runtimeEvents, adapter.SequencedRuntimeEvent{
					Sequence: chunk.Sequence,
					Event:    *chunk.RuntimeEvent,
				})
			}
			if chunk.Done {
				metadata = chunk.Metadata
				usage = chunk.Usage
				toolCalls = chunk.ToolCalls
				blocks = chunk.Blocks
				knowledgeHits = chunk.KnowledgeHits // U9：命中结构化透出
				memoryHits = chunk.MemoryHits
			}
		}
		trace.L(ctx).Info("流式消费完成", "content_len", content.Len())
		// v0.3.12 H4：改用 engine.StripAllThinking 统一剥离
		// 覆盖 <think>/<thinking>/<reasoning> 三种标签、任意位置（含中间嵌入）、多段、未闭合残段
		finalContent := engine.StripAllThinking(content.String())
		reply = &adapter.Reply{
			Content:             finalContent,
			Metadata:            metadata,
			Usage:               usage,
			ToolCalls:           toolCalls,
			Blocks:              blocks,
			KnowledgeHits:       knowledgeHits,
			MemoryHits:          memoryHits,
			AssistantMessageID:  assistantMessageID,
			BackendMessageID:    backendMessageID,
			MessageID:           messageID,
			LastSequence:        lastSequence,
			ReasoningDisclosure: reasoningDisclosure,
			RuntimeEvents:       runtimeEvents,
		}
	} else {
		var err error
		reply, err = s.engine.Process(ctx, msg)
		if err != nil {
			trace.L(ctx).Error("处理失败", "err", err)
			writeJSON(w, chatErrorStatus(req.Provider, err), map[string]string{
				"error": upstreamerr.PublicMessage(err, "error"),
			})
			return
		}
	}

	trace.L(ctx).Info("→ 回复", "content_len", len([]rune(reply.Content)), "elapsed_ms", time.Since(start).Milliseconds())

	// 返回响应
	canonical := reply.MessageContent
	if canonical == nil {
		canonical = canonicalChatContent(reply.Content, reply.Metadata)
	}
	writeJSON(w, http.StatusOK, ChatResponse{
		Reply:              reply.Content,
		MessageContent:     canonical,
		RenderManifest:     reply.RenderManifest,
		SessionID:          msg.SessionID,
		Metadata:           reply.Metadata,
		Usage:              reply.Usage,
		ToolCalls:          reply.ToolCalls,
		Blocks:             reply.Blocks,
		KnowledgeHits:      reply.KnowledgeHits,
		MemoryHits:         reply.MemoryHits,
		AssistantMessageID: reply.Metadata["assistant_message_id"],
		BackendMessageID:   reply.Metadata["backend_message_id"],
		MessageID:          reply.Metadata["message_id"],
	})
}

func chatErrorStatus(explicitProvider string, err error) int {
	var providerErr *engine.ProviderUnavailableError
	if strings.TrimSpace(explicitProvider) != "" && errors.As(err, &providerErr) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// handleChatSSE 处理 SSE 流式聊天请求（BUG-20260523-v2）。
//
// 把 ProcessStream 的 chunk channel 直接以 SSE 格式（`data: <json>\n\n`）写到 HTTP 响应，
// 让前端能逐 chunk 拿到内容（包括 reasoning / tool_calls 增量），
// 不再被迫等"完整推理时长"。
//
// 协议：
//   - 每个 chunk：`data: {"content":"...","reasoning":"...","done":false}\n\n`
//   - 错误 chunk：`data: {"error":"...","done":true}\n\n`
//   - 结束标记：`data: [DONE]\n\n`
//
// 每个节点都有结构化日志，便于排查（BUG-20260523 教训）。
func (s *Server) handleChatSSE(
	ctx context.Context,
	w http.ResponseWriter,
	msg *adapter.Message,
	se interface {
		ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error)
	},
	start time.Time,
) {
	if _, ok := w.(http.Flusher); !ok {
		trace.L(ctx).Error("[SSE] ResponseWriter 不支持 Flush — http.Server 配置异常")
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "server does not support streaming",
		})
		return
	}

	// sse.NewWriter sets the text/event-stream headers; the immediate Flush
	// commits a 200 and opens the stream before the first chunk arrives.
	writer := sse.MustNewWriter(w)
	writer.Flush()

	trace.L(ctx).Info("[SSE] 开始流式响应", "session", msg.SessionID, "user", msg.UserID)

	chunks, err := se.ProcessStream(ctx, msg)
	if err != nil {
		trace.L(ctx).Error("[SSE] ProcessStream 启动失败", "err", err)
		errPayload, _ := json.Marshal(map[string]any{
			"error": upstreamerr.PublicMessage(err, "error"),
			"done":  true,
		})
		_ = writer.WriteData(string(errPayload))
		return
	}

	var (
		chunkCount     int
		contentBytes   int
		reasoningBytes int
		toolCallCount  int
		hadError       bool
		canonical      strings.Builder
	)

	for chunk := range chunks {
		if chunk.Error != nil {
			hadError = true
			trace.L(ctx).Error("[SSE] chunk 错误", "err", chunk.Error, "chunks_so_far", chunkCount)
			errPayload, _ := json.Marshal(map[string]any{
				"error":                upstreamerr.PublicMessage(chunk.Error, "error"),
				"done":                 true,
				"assistant_message_id": chunk.AssistantMessageID,
				"backend_message_id":   chunk.BackendMessageID,
				"message_id":           chunk.MessageID,
				"sequence":             chunk.Sequence,
				"reasoning_disclosure": chunk.ReasoningDisclosure,
				"runtime_event":        chunk.RuntimeEvent,
			})
			_ = writer.WriteData(string(errPayload))
			return
		}

		chunkCount++
		contentBytes += len(chunk.Content)
		canonical.WriteString(chunk.Content)
		reasoningBytes += len(chunk.Reasoning)
		if len(chunk.ToolCalls) > 0 {
			toolCallCount = len(chunk.ToolCalls)
		}

		if chunk.Done {
			finalContent := engine.StripAllThinking(canonical.String())
			chunk.MessageContent = canonicalChatContent(finalContent, chunk.Metadata)
		}

		payload, err := json.Marshal(chunk)
		if err != nil {
			trace.L(ctx).Error("[SSE] 序列化 chunk 失败", "err", err, "chunks_so_far", chunkCount)
			continue
		}
		_ = writer.WriteData(string(payload))
	}

	if !hadError {
		_ = writer.WriteData(sse.OpenAIDoneToken)
	}

	trace.L(ctx).Info("[SSE] 流式响应结束",
		"chunks", chunkCount,
		"content_bytes", contentBytes,
		"reasoning_bytes", reasoningBytes,
		"tool_calls", toolCallCount,
		"had_error", hadError,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
}

const defaultDesktopUserID = "desktop-user"

type authenticatedHTTPPrincipal struct {
	userID   string
	platform adapter.Platform
}

type authenticatedHTTPPrincipalKey struct{}

func withAuthenticatedHTTPPrincipal(r *http.Request, principal authenticatedHTTPPrincipal) *http.Request {
	ctx := context.WithValue(r.Context(), authenticatedHTTPPrincipalKey{}, principal)
	ctx = skill.WithAuthenticatedUser(ctx, principal.userID)
	return r.WithContext(ctx)
}

func httpPrincipalFromRequest(r *http.Request) authenticatedHTTPPrincipal {
	if r != nil {
		if principal, ok := r.Context().Value(authenticatedHTTPPrincipalKey{}).(authenticatedHTTPPrincipal); ok &&
			principal.userID != "" {
			return principal
		}
	}
	// Direct handler calls in embedded/tests do not cross the HTTP auth
	// middleware. They are API calls and never inherit client identity claims.
	return authenticatedHTTPPrincipal{userID: "api-user", platform: adapter.PlatformAPI}
}

func resolveChatPlatform(_ ChatRequest, r *http.Request) (adapter.Platform, error) {
	return httpPrincipalFromRequest(r).platform, nil
}

// corsMiddleware 处理跨域请求
//
// 允许 Tauri 桌面端 (tauri://localhost, http://tauri.localhost)
// 和本地开发服务 (http://localhost:* / http://127.0.0.1:*) 的跨域访问。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 允许 Tauri 和本地开发环境的 origin
		isLocalhost := isLoopbackOrigin(origin, "http://localhost:")
		isLoopback127 := isLoopbackOrigin(origin, "http://127.0.0.1:")
		if isDesktopOrigin(origin) ||
			isLocalhost ||
			isLoopback127 {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
			w.Header().Set("Access-Control-Max-Age", "3600")
		}

		// 预检请求直接返回
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isDesktopOrigin(origin string) bool {
	return origin == "tauri://localhost" || origin == "http://tauri.localhost"
}

func isLoopbackOrigin(origin, prefix string) bool {
	if !strings.HasPrefix(origin, prefix) {
		return false
	}
	port := origin[len(prefix):]
	if len(port) == 0 || len(port) > 5 {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// apiAuthMiddleware 管理 API 认证中间件
//
// 对 /api/v1/ 下的管理写操作进行认证，日志和桌面端点也受保护。
// 如果配置了 APIToken，需要 Authorization: Bearer <token>。
// 为兼容本地桌面客户端和本机管理操作，localhost 请求始终允许访问。
// 非 localhost 请求在未配置 Token 时会被拒绝。
// isMountedScenarioPath 判断 path 是否落在某个已挂载的场景子路由前缀下（BUG-4）。
// 与 routes() 的挂载注册（mux.Handle(prefix+"/", ...)）同源：命中即须走场景鉴权守卫。
func (s *Server) isMountedScenarioPath(path string) bool {
	for _, m := range s.extraMounts {
		if strings.HasPrefix(path, m.prefix+"/") {
			return true
		}
	}
	return false
}

func (s *Server) apiAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if isPublicAPIRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		internalDesktop := strings.HasPrefix(path, "/api/internal/desktop/")
		needsAuth := strings.HasPrefix(path, "/api/v1/") || internalDesktop || path == "/ws" || s.isMountedScenarioPath(path)
		if !needsAuth {
			next.ServeHTTP(w, r)
			return
		}

		if internalDesktop {
			if isLoopbackRequest(r) && s.sidecarCapabilityToken != "" && tokenMatchesBearer(r, s.sidecarCapabilityToken) {
				next.ServeHTTP(w, withAuthenticatedHTTPPrincipal(r, authenticatedHTTPPrincipal{
					userID: defaultDesktopUserID, platform: adapter.PlatformDesktop,
				}))
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "native sidecar capability required",
			})
			return
		}

		if tokenMatchesBearer(r, s.cfg.Server.APIToken) {
			next.ServeHTTP(w, withAuthenticatedHTTPPrincipal(r, authenticatedHTTPPrincipal{
				userID: "api-user", platform: adapter.PlatformAPI,
			}))
			return
		}
		if isLoopbackRequest(r) && tokenMatchesBearer(r, s.sidecarCapabilityToken) {
			next.ServeHTTP(w, withAuthenticatedHTTPPrincipal(r, authenticatedHTTPPrincipal{
				userID: defaultDesktopUserID, platform: adapter.PlatformDesktop,
			}))
			return
		}
		// Compatibility transition: old Desktop builds have no token transport.
		// The fallback is loopback-only and disappears automatically as soon as a
		// per-start capability is configured. It is never available remotely.
		if isLoopbackRequest(r) && s.sidecarCapabilityToken == "" {
			next.ServeHTTP(w, withAuthenticatedHTTPPrincipal(r, authenticatedHTTPPrincipal{
				userID: defaultDesktopUserID, platform: adapter.PlatformDesktop,
			}))
			return
		}

		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "未授权：需要有效的 capability token",
		})
	})
}

func tokenMatchesBearer(r *http.Request, token string) bool {
	if r == nil || token == "" {
		return false
	}
	expected := "Bearer " + token
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) == 1
}

func isPublicAPIRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	path := r.URL.Path
	if r.Method == http.MethodOptions || (r.Method == http.MethodGet && path == "/health") ||
		(r.Method == http.MethodGet && path == "/api/v1/version") {
		return true
	}
	if r.Method == http.MethodPost && hasExactPathSegments(path, "/api/v1/webhooks/", 1) {
		return true
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodPost) &&
		hasExactPathSegments(path, "/api/v1/platforms/hooks/", 2)
}

func hasExactPathSegments(path, prefix string, count int) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) != count {
		return false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		logger.Error("writeJSON encode error", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("{\"error\":\"响应序列化失败\"}\n"))
		return
	}
	writeJSONBytes(w, status, body)
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte{'\n'})
}
