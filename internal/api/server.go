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

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/canvas"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/desktop"
	"github.com/everyday-items/hexclaw/internal/cron"
	"github.com/everyday-items/hexclaw/internal/engine"
	"github.com/everyday-items/hexclaw/internal/gateway"
	"github.com/everyday-items/hexclaw/internal/knowledge"
	"github.com/everyday-items/hexclaw/internal/memory"
	hexmcp "github.com/everyday-items/hexclaw/internal/mcp"
	"github.com/everyday-items/hexclaw/internal/router"
	"github.com/everyday-items/hexclaw/internal/skill/marketplace"
	"github.com/everyday-items/hexclaw/internal/voice"
	"github.com/everyday-items/hexclaw/internal/webhook"
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
	agentRouter *router.Router            // 多 Agent 路由器（可选）
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
func (s *Server) SetAgentRouter(r *router.Router) {
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

// --- 知识库 API ---

// AddDocumentRequest 添加文档请求
type AddDocumentRequest struct {
	Title   string `json:"title"`   // 文档标题
	Content string `json:"content"` // 文档内容
	Source  string `json:"source"`  // 来源（可选）
}

// handleAddDocument 添加文档到知识库
func (s *Server) handleAddDocument(w http.ResponseWriter, r *http.Request) {
	var req AddDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "title 和 content 不能为空",
		})
		return
	}

	doc, err := s.kb.AddDocument(r.Context(), req.Title, req.Content, req.Source)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          doc.ID,
		"title":       doc.Title,
		"chunk_count": doc.ChunkCount,
		"created_at":  doc.CreatedAt,
	})
}

// handleListDocuments 列出知识库文档
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.kb.ListDocuments(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取文档列表失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"documents": docs,
		"total":     len(docs),
	})
}

// handleDeleteDocument 删除知识库文档
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	if docID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文档 ID 不能为空",
		})
		return
	}

	if err := s.kb.DeleteDocument(r.Context(), docID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "文档已删除",
	})
}

// SearchKnowledgeRequest 知识库搜索请求
type SearchKnowledgeRequest struct {
	Query string `json:"query"` // 搜索查询
	TopK  int    `json:"top_k"` // 返回条数（默认 3）
}

// handleSearchKnowledge 搜索知识库
func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	var req SearchKnowledgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "query 不能为空",
		})
		return
	}

	topK := req.TopK
	if topK <= 0 {
		topK = 3
	}

	result, err := s.kb.Query(r.Context(), req.Query, topK)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

// --- 角色 API ---

// handleListRoles 列出可用 Agent 角色
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	// 从引擎获取工厂（通过类型断言）
	if eng, ok := s.engine.(*engine.ReActEngine); ok {
		factory := eng.AgentFactory()
		roles := factory.ListRoles()

		var roleList []map[string]string
		for _, name := range roles {
			role, _ := factory.GetRole(name)
			roleList = append(roleList, map[string]string{
				"name":  name,
				"title": role.Title,
				"goal":  role.Goal,
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"roles": roleList,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"roles": []string{},
	})
}

// --- Webhook API ---

// handleListWebhooks 列出用户的 Webhook
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}

	webhooks, err := s.webhookMgr.List(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取 Webhook 列表失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"webhooks": webhooks,
		"total":    len(webhooks),
	})
}

// RegisterWebhookRequest 注册 Webhook 请求
type RegisterWebhookRequest struct {
	Name   string `json:"name"`   // Webhook 名称（也是 URL 路径）
	Type   string `json:"type"`   // 类型: generic/github/gitlab
	Secret string `json:"secret"` // 签名验证 Secret
	Prompt string `json:"prompt"` // Agent 处理指令
	UserID string `json:"user_id"`
}

// handleRegisterWebhook 注册新 Webhook
func (s *Server) handleRegisterWebhook(w http.ResponseWriter, r *http.Request) {
	var req RegisterWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Name == "" || req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name 和 prompt 不能为空",
		})
		return
	}

	if req.UserID == "" {
		req.UserID = "api-user"
	}

	wh := &webhook.Webhook{
		Name:   req.Name,
		Type:   webhook.WebhookType(req.Type),
		Secret: req.Secret,
		Prompt: req.Prompt,
		UserID: req.UserID,
	}

	if err := s.webhookMgr.Register(r.Context(), wh); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "注册 Webhook 失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":   wh.ID,
		"name": wh.Name,
		"url":  fmt.Sprintf("/api/v1/webhooks/%s", wh.Name),
	})
}

// handleDeleteWebhook 删除 Webhook
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.webhookMgr.Unregister(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除 Webhook 失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Webhook 已删除"})
}

// --- Cron API ---

// AddCronJobRequest 添加定时任务请求
type AddCronJobRequest struct {
	Name     string `json:"name"`     // 任务名称
	Schedule string `json:"schedule"` // cron 表达式或 @every/@daily 等
	Prompt   string `json:"prompt"`   // Agent 处理指令
	UserID   string `json:"user_id"`
	Type     string `json:"type"`     // cron 或 once
}

// handleListCronJobs 列出定时任务
func (s *Server) handleListCronJobs(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}

	jobs, err := s.scheduler.ListJobs(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取任务列表失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":  jobs,
		"total": len(jobs),
	})
}

// handleAddCronJob 添加定时任务
func (s *Server) handleAddCronJob(w http.ResponseWriter, r *http.Request) {
	var req AddCronJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Name == "" || req.Schedule == "" || req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name、schedule 和 prompt 不能为空",
		})
		return
	}

	if req.UserID == "" {
		req.UserID = "api-user"
	}

	jobType := cron.JobTypeCron
	if req.Type == "once" {
		jobType = cron.JobTypeOnce
	}

	job := &cron.Job{
		Name:     req.Name,
		Type:     jobType,
		Schedule: req.Schedule,
		Prompt:   req.Prompt,
		UserID:   req.UserID,
	}

	if err := s.scheduler.AddJob(r.Context(), job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加任务失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          job.ID,
		"name":        job.Name,
		"next_run_at": job.NextRunAt,
	})
}

// handleDeleteCronJob 删除定时任务
func (s *Server) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.RemoveJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已删除"})
}

// handlePauseCronJob 暂停定时任务
func (s *Server) handlePauseCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.PauseJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "暂停任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已暂停"})
}

// handleResumeCronJob 恢复定时任务
func (s *Server) handleResumeCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.ResumeJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "恢复任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已恢复"})
}

// --- 文件记忆 API ---

// handleGetMemory 获取长期记忆内容
func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	content := s.fileMem.GetMemory()
	writeJSON(w, http.StatusOK, map[string]any{
		"content": content,
		"context": s.fileMem.LoadContext(),
	})
}

// SaveMemoryRequest 保存记忆请求
type SaveMemoryRequest struct {
	Content string `json:"content"` // 记忆内容
	Type    string `json:"type"`    // memory 或 daily
}

// handleSaveMemory 保存记忆
func (s *Server) handleSaveMemory(w http.ResponseWriter, r *http.Request) {
	var req SaveMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "content 不能为空",
		})
		return
	}

	var err error
	if req.Type == "daily" {
		err = s.fileMem.SaveDaily(req.Content)
	} else {
		err = s.fileMem.SaveMemory(req.Content)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "保存记忆失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "记忆已保存"})
}

// handleSearchMemory 搜索记忆
func (s *Server) handleSearchMemory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "q 参数不能为空",
		})
		return
	}

	results := s.fileMem.Search(query)
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"total":   len(results),
	})
}

// --- MCP API ---

// handleListMCPTools 列出所有已发现的 MCP 工具
func (s *Server) handleListMCPTools(w http.ResponseWriter, r *http.Request) {
	infos := s.mcpMgr.ToolInfos()
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": infos,
		"total": len(infos),
	})
}

// handleListMCPServers 列出已连接的 MCP Server
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	names := s.mcpMgr.ServerNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": names,
		"total":   len(names),
	})
}

// --- 技能市场 API ---

// handleListSkills 列出所有已安装的 Markdown 技能
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	skills := s.mp.List()

	var list []map[string]any
	for _, sk := range skills {
		list = append(list, map[string]any{
			"name":        sk.Meta.Name,
			"description": sk.Meta.Description,
			"author":      sk.Meta.Author,
			"version":     sk.Meta.Version,
			"triggers":    sk.Meta.Triggers,
			"tags":        sk.Meta.Tags,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"skills": list,
		"total":  len(list),
		"dir":    s.mp.Dir(),
	})
}

// InstallSkillRequest 安装技能请求
type InstallSkillRequest struct {
	Source string `json:"source"` // 源路径（本地文件/目录）
}

// handleInstallSkill 安装技能
func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var req InstallSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "source 不能为空",
		})
		return
	}

	sk, err := s.mp.Install(req.Source)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "安装技能失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":        sk.Meta.Name,
		"description": sk.Meta.Description,
		"version":     sk.Meta.Version,
		"message":     "技能已安装",
	})
}

// handleUninstallSkill 删除技能
func (s *Server) handleUninstallSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mp.Uninstall(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除技能失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "技能已删除"})
}

// --- 多 Agent 路由 API ---

// handleListAgents 列出已注册的 Agent
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.agentRouter.ListAgents()
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":  agents,
		"total":   len(agents),
		"default": s.agentRouter.DefaultAgent(),
	})
}

// RegisterAgentRequest 注册 Agent 请求
type RegisterAgentRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
}

// handleRegisterAgent 注册 Agent
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var req RegisterAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}

	err := s.agentRouter.Register(router.AgentConfig{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Model:       req.Model,
		Provider:    req.Provider,
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已注册", "name": req.Name})
}

// handleUnregisterAgent 注销 Agent
func (s *Server) handleUnregisterAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.agentRouter.Unregister(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已注销"})
}

// --- Canvas/A2UI API ---

// handleListPanels 列出所有活跃面板
func (s *Server) handleListPanels(w http.ResponseWriter, r *http.Request) {
	panels := s.canvasSvc.ListPanels()

	var list []map[string]any
	for _, p := range panels {
		list = append(list, map[string]any{
			"id":              p.ID,
			"title":           p.Title,
			"component_count": len(p.Components),
			"version":         p.Version,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"panels": list,
		"total":  len(list),
	})
}

// handleGetPanel 获取面板详情
func (s *Server) handleGetPanel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	panel, ok := s.canvasSvc.GetPanel(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "面板不存在"})
		return
	}
	writeJSON(w, http.StatusOK, panel)
}

// CanvasEventRequest Canvas 事件请求
type CanvasEventRequest struct {
	PanelID     string         `json:"panel_id"`
	ComponentID string         `json:"component_id"`
	Action      string         `json:"action"`
	Data        map[string]any `json:"data"`
}

// handleCanvasEvent 处理 Canvas 事件
func (s *Server) handleCanvasEvent(w http.ResponseWriter, r *http.Request) {
	var req CanvasEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	result, err := s.canvasSvc.HandleEvent(&canvas.Event{
		PanelID:     req.PanelID,
		ComponentID: req.ComponentID,
		Action:      req.Action,
		Data:        req.Data,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "事件处理失败: " + err.Error()})
		return
	}

	if result != nil {
		writeJSON(w, http.StatusOK, result)
	} else {
		writeJSON(w, http.StatusOK, map[string]string{"message": "事件已处理"})
	}
}

// --- 语音 API ---

// handleVoiceStatus 查看语音服务状态
func (s *Server) handleVoiceStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"stt_enabled":  s.voiceSvc.HasSTT(),
		"tts_enabled":  s.voiceSvc.HasTTS(),
		"stt_provider": s.voiceSvc.STTName(),
		"tts_provider": s.voiceSvc.TTSName(),
	})
}

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
