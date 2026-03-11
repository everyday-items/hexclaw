// Package api 提供 HexClaw HTTP API 服务
//
// 包含以下端点：
//   - GET  /health          健康检查
//   - POST /api/v1/chat     同步聊天
//   - GET  /api/v1/sessions 会话列表
//
// 服务器支持优雅关闭：收到 SIGINT/SIGTERM 后等待请求处理完毕再退出。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/canvas"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/cron"
	"github.com/everyday-items/hexclaw/desktop"
	"github.com/everyday-items/hexclaw/engine"
	"github.com/everyday-items/hexclaw/gateway"
	"github.com/everyday-items/hexclaw/knowledge"
	"github.com/everyday-items/hexclaw/memory"
	hexmcp "github.com/everyday-items/hexclaw/mcp"
	"github.com/everyday-items/hexclaw/router"
	"github.com/everyday-items/hexclaw/skill/marketplace"
	"github.com/everyday-items/hexclaw/voice"
	"github.com/everyday-items/hexclaw/webhook"
	"github.com/everyday-items/toolkit/util/idgen"
)

// Server HTTP API 服务器
type Server struct {
	cfg        *config.Config
	engine     engine.Engine
	gateway    gateway.Gateway
	kb         *knowledge.Manager  // 知识库管理器（可选）
	webhookMgr *webhook.Manager    // Webhook 管理器（可选）
	scheduler  *cron.Scheduler     // Cron 调度器（可选）
	fileMem    *memory.FileMemory         // 文件记忆（可选）
	mcpMgr      *hexmcp.Manager           // MCP 管理器（可选）
	mp          *marketplace.Marketplace  // 技能市场（可选）
	agentRouter *router.Dispatcher            // 多 Agent 路由器（可选）
	canvasSvc   *canvas.Service           // Canvas/A2UI 服务（可选）
	voiceSvc    *voice.Service            // 语音服务（可选）
	desktopSvc  *desktop.Service          // 桌面集成服务（可选）
	wsHandler   http.Handler              // WebSocket Handler（可选）
	server      *http.Server
}

// NewServer 创建 API 服务器
//
// gw 可为 nil，此时跳过安全检查（仅限开发模式）。
func NewServer(cfg *config.Config, eng engine.Engine, gw gateway.Gateway) *Server {
	return &Server{
		cfg:     cfg,
		engine:  eng,
		gateway: gw,
	}
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

// SetDesktop 设置桌面集成服务
//
// 设置后启用桌面通知、剪贴板等 API。
func (s *Server) SetDesktop(svc *desktop.Service) {
	s.desktopSvc = svc
}

// Start 启动 HTTP 服务器
//
// 注册路由并开始监听。此方法会阻塞直到服务器停止。
// 使用 Stop() 方法触发优雅关闭。
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /health", s.handleHealth)

	// API v1
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)

	// 知识库 API
	if s.kb != nil {
		mux.HandleFunc("POST /api/v1/knowledge/documents", s.handleAddDocument)
		mux.HandleFunc("GET /api/v1/knowledge/documents", s.handleListDocuments)
		mux.HandleFunc("DELETE /api/v1/knowledge/documents/{id}", s.handleDeleteDocument)
		mux.HandleFunc("POST /api/v1/knowledge/search", s.handleSearchKnowledge)
	}

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
	}

	// 文件记忆 API
	if s.fileMem != nil {
		mux.HandleFunc("GET /api/v1/memory", s.handleGetMemory)
		mux.HandleFunc("POST /api/v1/memory", s.handleSaveMemory)
		mux.HandleFunc("GET /api/v1/memory/search", s.handleSearchMemory)
	}

	// MCP API
	if s.mcpMgr != nil {
		mux.HandleFunc("GET /api/v1/mcp/tools", s.handleListMCPTools)
		mux.HandleFunc("GET /api/v1/mcp/servers", s.handleListMCPServers)
	}

	// 技能市场 API
	if s.mp != nil {
		mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)
		mux.HandleFunc("POST /api/v1/skills/install", s.handleInstallSkill)
		mux.HandleFunc("DELETE /api/v1/skills/{name}", s.handleUninstallSkill)
	}

	// 多 Agent 路由 API
	if s.agentRouter != nil {
		mux.HandleFunc("GET /api/v1/agents", s.handleListAgents)
		mux.HandleFunc("POST /api/v1/agents", s.handleRegisterAgent)
		mux.HandleFunc("DELETE /api/v1/agents/{name}", s.handleUnregisterAgent)
	}

	// Canvas/A2UI API
	if s.canvasSvc != nil {
		mux.HandleFunc("GET /api/v1/canvas/panels", s.handleListPanels)
		mux.HandleFunc("GET /api/v1/canvas/panels/{id}", s.handleGetPanel)
		mux.HandleFunc("POST /api/v1/canvas/events", s.handleCanvasEvent)
	}

	// 语音 API
	if s.voiceSvc != nil {
		mux.HandleFunc("GET /api/v1/voice/status", s.handleVoiceStatus)
	}

	// 桌面集成 API
	if s.desktopSvc != nil {
		s.desktopSvc.RegisterRoutes(mux)
	}

	// WebSocket（Web UI）
	if s.wsHandler != nil {
		mux.Handle("/ws", s.wsHandler)
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second, // 流式输出需要更长的超时
		IdleTimeout:       120 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	log.Printf("HTTP 服务启动: %s", addr)
	return s.server.ListenAndServe()
}

// Stop 优雅关闭服务器
//
// 等待所有正在处理的请求完成（最多等待 30 秒）
func (s *Server) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
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
	SessionID string `json:"session_id,omitempty"`  // 会话 ID（可选，空则创建新会话）
	UserID    string `json:"user_id,omitempty"`     // 用户 ID（可选）
	Role      string `json:"role,omitempty"`        // Agent 角色（可选：assistant/researcher/writer/coder/translator/analyst）
}

// ChatResponse 聊天回复
type ChatResponse struct {
	Reply     string            `json:"reply"`                // 回复内容
	SessionID string            `json:"session_id"`           // 会话 ID
	Metadata  map[string]string `json:"metadata,omitempty"`   // 元数据
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

	// 解析请求
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// 调用引擎处理
	reply, err := s.engine.Process(r.Context(), msg)
	if err != nil {
		log.Printf("处理消息失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "处理消息失败，请稍后重试",
		})
		return
	}

	// 返回响应
	writeJSON(w, http.StatusOK, ChatResponse{
		Reply:     reply.Content,
		SessionID: msg.SessionID,
		Metadata:  reply.Metadata,
	})
}


// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
