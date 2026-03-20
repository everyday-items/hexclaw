// Package api 提供 HexClaw HTTP API 服务
//
// 包含以下端点：
//   - GET    /health                        健康检查
//   - POST   /api/v1/chat                   同步聊天
//   - GET    /api/v1/sessions               会话列表
//   - GET    /api/v1/sessions/{id}          会话详情
//   - DELETE /api/v1/sessions/{id}          删除会话
//   - GET    /api/v1/sessions/{id}/messages 消息历史
//   - GET    /api/v1/sessions/{id}/branches 会话分支列表
//   - POST   /api/v1/sessions/{id}/fork     创建对话分支
//   - GET    /api/v1/messages/search        全文搜索消息
//
// 服务器支持优雅关闭：收到 SIGINT/SIGTERM 后等待请求处理完毕再退出。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/desktop"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/gateway"
	"github.com/hexagon-codes/hexclaw/instances"
	"github.com/hexagon-codes/hexclaw/knowledge"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/hexclaw/voice"
	"github.com/hexagon-codes/hexclaw/webhook"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Server HTTP API 服务器
type Server struct {
	cfg           *config.Config
	engine        engine.Engine
	gateway       gateway.Gateway
	store         storage.Store            // 数据存储层
	kb            *knowledge.Manager       // 知识库管理器（可选）
	webhookMgr    *webhook.Manager         // Webhook 管理器（可选）
	scheduler     *cron.Scheduler          // Cron 调度器（可选）
	fileMem       *memory.FileMemory       // 文件记忆（可选）
	mcpMgr        *hexmcp.Manager          // MCP 管理器（可选）
	mp            *marketplace.Marketplace // 技能市场（可选）
	agentRouter   *router.Dispatcher       // 多 Agent 路由器（可选）
	agentStore    router.Store             // Agent/Rule 持久化（可选）
	instanceMgr   *instances.Manager       // 平台实例运行时（可选）
	canvasSvc     *canvas.Service          // Canvas/A2UI 服务（可选）
	voiceSvc      *voice.Service           // 语音服务（可选）
	desktopSvc    *desktop.Service         // 桌面集成服务（可选）
	wsHandler     http.Handler             // WebSocket Handler（可选）
	logCollector  *LogCollector            // 日志收集器
	workflowStore *WorkflowStore           // 工作流存储
	teamStore     *TeamStore               // 团队数据存储
	version       string                   // 版本号
	server        *http.Server
	statsMu       sync.Mutex
	statsCache    statsResponse
	statsJSON     []byte
	statsCacheAt  time.Time
}

// NewServer 创建 API 服务器
//
// gw 可为 nil，此时跳过安全检查（仅限开发模式）。
// store 可为 nil，此时会话/搜索/分支 API 不可用。
func NewServer(cfg *config.Config, eng engine.Engine, gw gateway.Gateway, store storage.Store) *Server {
	return &Server{
		cfg:           cfg,
		engine:        eng,
		gateway:       gw,
		store:         store,
		logCollector:  NewLogCollector(5000),
		workflowStore: NewWorkflowStore(),
		teamStore:     NewTeamStore(defaultDataDir()),
	}
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

// SetMarketplace 设置技能市场
//
// 设置后启用技能安装/列表/删除 API。
func (s *Server) SetMarketplace(mp *marketplace.Marketplace) {
	s.mp = mp
}

// SetAgentRouter 设置多 Agent 路由器
//
// 设置后启用 Agent 路由管理 API。
func (s *Server) SetAgentRouter(r *router.Dispatcher) {
	s.agentRouter = r
}

// SetAgentStore 设置 Agent/Rule 持久化层
func (s *Server) SetAgentStore(store router.Store) {
	s.agentStore = store
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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /health", s.handleHealth)

	// API v1
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)

	// 知识库 API
	if s.kb != nil {
		mux.HandleFunc("POST /api/v1/knowledge/documents", s.handleAddDocument)
		mux.HandleFunc("POST /api/v1/knowledge/upload", s.handleUploadDocument)
		mux.HandleFunc("GET /api/v1/knowledge/documents", s.handleListDocuments)
		mux.HandleFunc("DELETE /api/v1/knowledge/documents/{id}", s.handleDeleteDocument)
		mux.HandleFunc("POST /api/v1/knowledge/documents/{id}/reindex", s.handleReindexDocument)
		mux.HandleFunc("POST /api/v1/knowledge/search", s.handleSearchKnowledge)
	} else {
		mux.HandleFunc("GET /api/v1/knowledge/documents", emptyList("documents"))
	}

	// 会话 / 搜索 / 分支 API
	if s.store != nil {
		mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
		mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
		mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
		mux.HandleFunc("GET /api/v1/sessions/{id}/messages", s.handleListMessages)
		mux.HandleFunc("GET /api/v1/sessions/{id}/branches", s.handleListBranches)
		mux.HandleFunc("POST /api/v1/sessions/{id}/fork", s.handleForkSession)
		mux.HandleFunc("GET /api/v1/messages/search", s.handleSearchMessages)
	}

	// 配置 API
	mux.HandleFunc("GET /api/v1/config/llm", s.handleGetLLMConfig)
	mux.HandleFunc("PUT /api/v1/config/llm", s.handleUpdateLLMConfig)
	mux.HandleFunc("POST /api/v1/config/llm/test", s.handleTestLLMConfig)

	// 角色列表 API
	mux.HandleFunc("GET /api/v1/roles", s.handleListRoles)

	// Webhook API
	if s.webhookMgr != nil {
		mux.HandleFunc("POST /api/v1/webhooks/{name}", s.webhookMgr.Handler())
		mux.HandleFunc("GET /api/v1/webhooks", s.handleListWebhooks)
		mux.HandleFunc("POST /api/v1/webhooks", s.handleRegisterWebhook)
		mux.HandleFunc("DELETE /api/v1/webhooks/{name}", s.handleDeleteWebhook)
	}

	// Cron API
	if s.scheduler != nil {
		mux.HandleFunc("GET /api/v1/cron/jobs", s.handleListCronJobs)
		mux.HandleFunc("POST /api/v1/cron/jobs", s.handleAddCronJob)
		mux.HandleFunc("DELETE /api/v1/cron/jobs/{id}", s.handleDeleteCronJob)
		mux.HandleFunc("POST /api/v1/cron/jobs/{id}/pause", s.handlePauseCronJob)
		mux.HandleFunc("POST /api/v1/cron/jobs/{id}/resume", s.handleResumeCronJob)
		mux.HandleFunc("POST /api/v1/cron/jobs/{id}/trigger", s.handleTriggerCronJob)
		mux.HandleFunc("GET /api/v1/cron/jobs/{id}/history", s.handleCronJobHistory)
	}

	// 文件记忆 API
	if s.fileMem != nil {
		mux.HandleFunc("GET /api/v1/memory", s.handleGetMemory)
		mux.HandleFunc("POST /api/v1/memory", s.handleSaveMemory)
		mux.HandleFunc("PUT /api/v1/memory", s.handleUpdateMemory)
		mux.HandleFunc("DELETE /api/v1/memory", s.handleDeleteMemory)
		mux.HandleFunc("DELETE /api/v1/memory/{id}", s.handleDeleteMemoryItem)
		mux.HandleFunc("GET /api/v1/memory/search", s.handleSearchMemory)
	} else {
		mux.HandleFunc("GET /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"content": "", "type": "memory"})
		})
		mux.HandleFunc("PUT /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"message": "记忆模块未启用"})
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
		mux.HandleFunc("PUT /api/v1/skills/{name}/status", s.handleSkillStatus)
		mux.HandleFunc("POST /api/v1/skills/install", s.handleInstallSkill)
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
		mux.HandleFunc("GET /api/v1/platforms/instances", s.handleListInstances)
		mux.HandleFunc("GET /api/v1/platforms/instances/health", s.handleListInstanceHealth)
		mux.HandleFunc("POST /api/v1/platforms/instances", s.handleUpsertInstance)
		mux.HandleFunc("PUT /api/v1/platforms/instances/{name}", s.handleUpsertInstance)
		mux.HandleFunc("DELETE /api/v1/platforms/instances/{name}", s.handleDeleteInstance)
		mux.HandleFunc("GET /api/v1/platforms/instances/{name}/health", s.handleGetInstanceHealth)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/test", s.handleTestInstance)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/start", s.handleStartInstance)
		mux.HandleFunc("POST /api/v1/platforms/instances/{name}/stop", s.handleStopInstance)
		mux.HandleFunc("POST /api/v1/im/channels/{provider}/test", s.handleTestChannelConfig)
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

	// 语音 API
	if s.voiceSvc != nil {
		mux.HandleFunc("GET /api/v1/voice/status", s.handleVoiceStatus)
		mux.HandleFunc("POST /api/v1/voice/transcribe", s.handleVoiceTranscribe)
		mux.HandleFunc("POST /api/v1/voice/synthesize", s.handleVoiceSynthesize)
	}

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

	// ClawHub 搜索（Skill 市场）
	mux.HandleFunc("GET /api/v1/clawhub/search", s.handleClawHubSearch)

	// Team API（共享 Agent + 团队成员）
	mux.HandleFunc("GET /api/v1/team/agents", s.handleListSharedAgents)
	mux.HandleFunc("POST /api/v1/team/agents", s.handleShareAgent)
	mux.HandleFunc("DELETE /api/v1/team/agents/{id}", s.handleDeleteSharedAgent)
	mux.HandleFunc("GET /api/v1/team/members", s.handleListTeamMembers)
	mux.HandleFunc("POST /api/v1/team/members", s.handleInviteTeamMember)
	mux.HandleFunc("DELETE /api/v1/team/members/{id}", s.handleRemoveTeamMember)

	// 桌面集成 API
	if s.desktopSvc != nil {
		s.desktopSvc.RegisterRoutes(mux)
	}

	// WebSocket（Web UI）
	if s.wsHandler != nil {
		mux.Handle("/ws", s.wsHandler)
	}

	// 管理 API 认证中间件
	return s.apiAuthMiddleware(corsMiddleware(mux))
}

// Start 启动 HTTP 服务器
//
// 注册路由并开始监听。此方法会阻塞直到服务器停止。
// 使用 Stop() 方法触发优雅关闭。
func (s *Server) Start(ctx context.Context) error {
	handler := s.routes()
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second, // 流式输出需要更长的超时
		IdleTimeout:       120 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	return s.server.ListenAndServe()
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
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
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
	Message   string `json:"message"`              // 用户消息内容
	SessionID string `json:"session_id,omitempty"` // 会话 ID（可选，空则创建新会话）
	UserID    string `json:"user_id,omitempty"`    // 用户 ID（可选）
	Role      string `json:"role,omitempty"`       // Agent 角色（可选：assistant/researcher/writer/coder/translator/analyst）
}

// ChatResponse 聊天回复
type ChatResponse struct {
	Reply     string              `json:"reply"`                // 回复内容
	SessionID string              `json:"session_id"`           // 会话 ID
	Metadata  map[string]string   `json:"metadata,omitempty"`   // 元数据
	Usage     *adapter.Usage      `json:"usage,omitempty"`      // Token 使用统计
	ToolCalls []adapter.ToolCall  `json:"tool_calls,omitempty"` // 工具调用记录
}

// handleChat 同步聊天端点
//
// 请求体: {"message": "你好", "session_id": "optional", "user_id": "optional"}
// 响应: {"reply": "你好！有什么可以帮助你的？", "session_id": "sess-xxx"}
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "引擎未就绪",
		})
		return
	}

	// 解析请求（限制请求体大小为 1MB，防止 OOM 攻击）
	const maxRequestBodySize = 1 << 20 // 1MB
	var req ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodySize)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "message 不能为空",
		})
		return
	}

	// 构建统一消息
	userID := req.UserID
	if userID == "" {
		userID = "api-user" // API 调用的默认用户
	}

	msg := &adapter.Message{
		ID:        "msg-" + idgen.ShortID(),
		Platform:  adapter.PlatformAPI,
		UserID:    userID,
		UserName:  userID,
		SessionID: req.SessionID,
		Content:   req.Message,
		Timestamp: time.Now(),
		Metadata:  make(map[string]string),
	}

	// 如果指定了角色，通过元数据传递给引擎
	if req.Role != "" {
		msg.Metadata["role"] = req.Role
	}

	// 安全网关检查
	if s.gateway != nil {
		if err := s.gateway.Check(r.Context(), msg); err != nil {
			if gwErr, ok := err.(*gateway.GatewayError); ok {
				log.Printf("安全检查拒绝: layer=%s code=%s user=%s", gwErr.Layer, gwErr.Code, msg.UserID)
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

	// 调用引擎处理（日志只记录长度，不记录原文，防止 PII 泄露）
	s.logCollector.Info("chat", fmt.Sprintf("← %s (%d 字符)", userID, len([]rune(req.Message))))
	reply, err := s.engine.Process(r.Context(), msg)
	if err != nil {
		s.logCollector.Error("chat", fmt.Sprintf("处理失败: user=%s err=%v", userID, err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "处理消息失败，请稍后重试",
		})
		return
	}

	s.logCollector.Info("chat", fmt.Sprintf("→ %s (%d 字符)", userID, len([]rune(reply.Content))))

	// 返回响应
	writeJSON(w, http.StatusOK, ChatResponse{
		Reply:     reply.Content,
		SessionID: msg.SessionID,
		Metadata:  reply.Metadata,
		Usage:     reply.Usage,
		ToolCalls: reply.ToolCalls,
	})
}

// corsMiddleware 处理跨域请求
//
// 允许 Tauri 桌面端 (tauri://localhost, http://tauri.localhost)
// 和本地开发服务 (http://localhost:*) 的跨域访问。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 允许 Tauri 和本地开发环境的 origin
		isLocalhost := false
		if strings.HasPrefix(origin, "http://localhost:") {
			// 确保端口部分是纯数字，防止 http://localhost:evil.com 绕过
			port := origin[17:]
			isLocalhost = len(port) > 0 && len(port) <= 5
			for _, c := range port {
				if c < '0' || c > '9' {
					isLocalhost = false
					break
				}
			}
		}
		if origin == "tauri://localhost" ||
			origin == "http://tauri.localhost" ||
			isLocalhost {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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

// apiAuthMiddleware 管理 API 认证中间件
//
// 对 /api/v1/ 下的写操作（POST/PUT/DELETE）进行认证。
// 如果配置了 APIToken，需要 Authorization: Bearer <token>。
// 未配置 Token 时，仅允许 localhost 访问管理端点。
// GET 请求和 /health、/ws 不需要认证。
func (s *Server) apiAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 认证规则：
		// 1. 所有写操作（POST/PUT/DELETE）需认证，除 /api/v1/chat 和 webhook 接收
		// 2. 日志 API（GET /api/v1/logs*）需认证（可能含敏感信息）
		path := r.URL.Path
		isWriteOp := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete
		isWebhookReceiver := (r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v1/webhooks/") && path != "/api/v1/webhooks") ||
			((r.Method == http.MethodGet || r.Method == http.MethodPost) &&
				strings.HasPrefix(path, "/api/v1/platforms/hooks/"))
		isLogsAPI := path == "/api/v1/logs" || strings.HasPrefix(path, "/api/v1/logs/")
		needsAuth := isLogsAPI || (isWriteOp && strings.HasPrefix(path, "/api/v1/") && path != "/api/v1/chat" && !isWebhookReceiver)

		if !needsAuth {
			next.ServeHTTP(w, r)
			return
		}

		token := s.cfg.Server.APIToken
		if token != "" {
			// 配置了 Token：验证 Authorization header（constant-time 防时序攻击）
			auth := r.Header.Get("Authorization")
			expected := "Bearer " + token
			if subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "未授权：需要有效的 API Token",
				})
				return
			}
		} else {
			// 未配置 Token：仅允许 localhost
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if host != "127.0.0.1" && host != "::1" && host != "localhost" {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "未配置 API Token，仅允许本地访问管理端点",
				})
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		log.Printf("writeJSON encode error: %v", err)
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
