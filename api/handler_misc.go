package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/engine"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/voice"
)

type skillRuntimeController interface {
	SetSkillEnabled(name string, enabled bool) error
	SkillEnabled(name string) (bool, bool)
}

type skillStatusResponse struct {
	Name             string   `json:"name,omitempty"`
	Description      string   `json:"description,omitempty"`
	Author           string   `json:"author,omitempty"`
	Version          string   `json:"version,omitempty"`
	Triggers         []string `json:"triggers,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Enabled          bool     `json:"enabled"`
	EffectiveEnabled bool     `json:"effective_enabled"`
	RequiresRestart  bool     `json:"requires_restart"`
	Message          string   `json:"message,omitempty"`
}

// --- 角色 API ---

// handleListRoles 列出可用 Agent 角色
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	if eng, ok := s.engine.(*engine.ReActEngine); ok {
		factory := eng.AgentFactory()
		roles := factory.ListRoles()

		roleList := make([]map[string]any, 0, len(roles))
		for _, name := range roles {
			role, _ := factory.GetRole(name)
			roleList = append(roleList, map[string]any{
				"name":        name,
				"title":       role.Title,
				"goal":        role.Goal,
				"backstory":   role.Backstory,
				"expertise":   role.Expertise,
				"tools":       role.Tools,
				"constraints": role.Constraints,
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"roles": roleList,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"roles": []map[string]any{},
	})
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

// handleAddMCPServer 动态添加 MCP Server
func (s *Server) handleAddMCPServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		Command   string   `json:"command"`
		Args      []string `json:"args"`
		Transport string   `json:"transport"`
		Endpoint  string   `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}

	transport := req.Transport
	if transport == "" {
		if req.Endpoint != "" {
			transport = "sse"
		} else {
			transport = "stdio"
		}
	}

	if transport == "stdio" && req.Command == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stdio 模式需要指定 command"})
		return
	}
	if transport == "sse" && req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sse 模式需要指定 endpoint"})
		return
	}

	cfg := hexmcp.ServerConfig{
		Name:      req.Name,
		Transport: transport,
		Command:   req.Command,
		Args:      req.Args,
		Endpoint:  req.Endpoint,
		Enabled:   true,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := s.mcpMgr.AddServer(ctx, cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("MCP Server %q 已添加", req.Name)})
}

// handleRemoveMCPServer 动态移除 MCP Server
func (s *Server) handleRemoveMCPServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server name 不能为空"})
		return
	}

	if err := s.mcpMgr.RemoveServer(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("MCP Server %q 已移除", name)})
}

// --- 技能市场 API ---

// handleListSkills 列出所有已安装的 Markdown 技能
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	skills := s.mp.List()

	list := make([]skillStatusResponse, 0, len(skills))
	for _, sk := range skills {
		enabled := s.mp.IsEnabled(sk.Meta.Name)
		effective, requiresRestart, message := s.skillEffectiveState(sk.Meta.Name, enabled)
		list = append(list, skillStatusResponse{
			Name:             sk.Meta.Name,
			Description:      sk.Meta.Description,
			Author:           sk.Meta.Author,
			Version:          sk.Meta.Version,
			Triggers:         sk.Meta.Triggers,
			Tags:             sk.Meta.Tags,
			Enabled:          enabled,
			EffectiveEnabled: effective,
			RequiresRestart:  requiresRestart,
			Message:          message,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"skills": list,
		"total":  len(list),
		"dir":    s.mp.Dir(),
	})
}

// SkillStatusRequest 技能状态请求
type SkillStatusRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSkillStatus 设置技能启用/禁用状态
func (s *Server) handleSkillStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}
	if filepath.Base(name) != name || strings.Contains(name, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法技能名称"})
		return
	}
	if _, ok := s.mp.Get(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "技能未安装"})
		return
	}
	var req SkillStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := s.mp.SetEnabled(name, req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存状态失败: " + err.Error()})
		return
	}

	effective := req.Enabled
	requiresRestart := false
	message := "技能状态已更新并立即生效"
	if runtime, ok := s.engine.(skillRuntimeController); ok {
		if err := runtime.SetSkillEnabled(name, req.Enabled); err != nil {
			requiresRestart = true
			message = "技能状态已保存，但当前运行时未生效: " + err.Error()
			if current, exists := runtime.SkillEnabled(name); exists {
				effective = current
			} else {
				effective = false
			}
		}
	} else {
		requiresRestart = true
		message = "技能状态已保存，当前运行时不支持热更新，重启后生效"
	}

	writeJSON(w, http.StatusOK, skillStatusResponse{
		Enabled:          req.Enabled,
		EffectiveEnabled: effective,
		RequiresRestart:  requiresRestart,
		Message:          message,
	})
}

// InstallSkillRequest 安装技能请求
type InstallSkillRequest struct {
	Source string `json:"source"` // 源路径（本地文件/目录）
}

// handleInstallSkill 安装技能
//
// source 支持两种格式:
//   - clawhub://skill-name  — 从 ClawHub 在线安装
//   - 本地相对路径           — 从本地文件系统安装
func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var req InstallSkillRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

	// ClawHub 在线安装
	if strings.HasPrefix(req.Source, "clawhub://") {
		skillName := strings.TrimPrefix(req.Source, "clawhub://")
		if s.skillHub == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "ClawHub 未启用",
			})
			return
		}
		if s.skillHub.GetCatalog() == nil {
			if err := s.skillHub.Refresh(r.Context()); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"error": "获取 ClawHub 技能目录失败: " + err.Error(),
				})
				return
			}
		}
		if err := s.skillHub.Install(r.Context(), skillName); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "安装技能失败: " + err.Error(),
			})
			return
		}
		// Reload marketplace so the new skill appears in List()
		_ = s.mp.Init()
		s.syncEngineMarketplaceSkills()
		writeJSON(w, http.StatusOK, map[string]any{
			"name":              skillName,
			"message":           "技能已从 ClawHub 安装并已同步到运行引擎",
			"requires_restart":  false,
			"runtime_registered": true,
		})
		return
	}

	// 本地安装: 禁止绝对路径和路径穿越
	if filepath.IsAbs(req.Source) || strings.Contains(req.Source, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "source 路径不安全",
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

	s.syncEngineMarketplaceSkills()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               sk.Meta.Name,
		"description":        sk.Meta.Description,
		"version":            sk.Meta.Version,
		"message":            "技能已安装并已同步到运行引擎",
		"requires_restart":   false,
		"runtime_registered": true,
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
	s.syncEngineMarketplaceSkills()
	writeJSON(w, http.StatusOK, map[string]string{"message": "技能已删除并已同步运行引擎"})
}

func (s *Server) skillEffectiveState(name string, enabled bool) (bool, bool, string) {
	runtime, ok := s.engine.(skillRuntimeController)
	if !ok {
		if enabled {
			return enabled, true, "当前运行时不支持技能状态探测，可能需要重启后生效"
		}
		return enabled, false, ""
	}

	effective, exists := runtime.SkillEnabled(name)
	if !exists {
		if enabled {
			return false, true, "技能已安装，但当前运行时未注册，重启后生效"
		}
		return false, false, ""
	}
	if effective != enabled {
		return effective, true, "配置已保存，但运行时状态尚未与持久化配置对齐"
	}
	return effective, false, ""
}

// --- 多 Agent 路由 API ---

// handleListAgents 列出已注册的 Agent 和路由规则
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	if s.agentRouter == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"agents":  []any{},
			"rules":   []any{},
			"total":   0,
			"default": "",
		})
		return
	}
	agents := s.agentRouter.ListAgents()
	rules := s.agentRouter.ListRules()
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":  agents,
		"rules":   rules,
		"total":   len(agents),
		"default": s.agentRouter.DefaultAgent(),
	})
}

// RegisterAgentRequest 注册/更新 Agent 请求
type RegisterAgentRequest struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name"`
	Description  string            `json:"description"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	SystemPrompt string            `json:"system_prompt"`
	Skills       []string          `json:"skills"`
	MaxTokens    int               `json:"max_tokens"`
	Temperature  float64           `json:"temperature"`
	Metadata     map[string]string `json:"metadata"`
}

type UpdateAgentRequest struct {
	DisplayName  *string            `json:"display_name"`
	Description  *string            `json:"description"`
	Model        *string            `json:"model"`
	Provider     *string            `json:"provider"`
	SystemPrompt *string            `json:"system_prompt"`
	Skills       *[]string          `json:"skills"`
	MaxTokens    *int               `json:"max_tokens"`
	Temperature  *float64           `json:"temperature"`
	Metadata     *map[string]string `json:"metadata"`
}

// handleRegisterAgent 注册 Agent（内存 + 持久化）
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var req RegisterAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 不能为空"})
		return
	}

	cfg := router.AgentConfig{
		Name:         req.Name,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Model:        req.Model,
		Provider:     req.Provider,
		SystemPrompt: req.SystemPrompt,
		Skills:       req.Skills,
		MaxTokens:    req.MaxTokens,
		Temperature:  req.Temperature,
		Metadata:     req.Metadata,
	}

	if err := s.agentRouter.Register(cfg); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if s.agentStore != nil {
		_ = s.agentStore.SaveAgent(r.Context(), &cfg)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已注册", "name": req.Name})
}

// handleUpdateAgent 更新 Agent 配置（内存 + 持久化）
func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req UpdateAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	existing, ok := s.agentRouter.GetAgent(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent \"" + name + "\" 未注册"})
		return
	}
	cfg := *existing
	cfg.Name = name
	if req.DisplayName != nil {
		cfg.DisplayName = *req.DisplayName
	}
	if req.Description != nil {
		cfg.Description = *req.Description
	}
	if req.Model != nil {
		cfg.Model = *req.Model
	}
	if req.Provider != nil {
		cfg.Provider = *req.Provider
	}
	if req.SystemPrompt != nil {
		cfg.SystemPrompt = *req.SystemPrompt
	}
	if req.Skills != nil {
		cfg.Skills = *req.Skills
	}
	if req.MaxTokens != nil {
		cfg.MaxTokens = *req.MaxTokens
	}
	if req.Temperature != nil {
		cfg.Temperature = *req.Temperature
	}
	if req.Metadata != nil {
		cfg.Metadata = *req.Metadata
	}
	if err := s.agentRouter.UpdateAgent(cfg); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if s.agentStore != nil {
		updated, ok := s.agentRouter.GetAgent(name)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新后读取 Agent 失败"})
			return
		}
		if err := s.agentStore.SaveAgent(r.Context(), updated); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已更新", "name": name})
}

// handleUnregisterAgent 注销 Agent（内存 + 持久化）
func (s *Server) handleUnregisterAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.agentRouter.Unregister(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if s.agentStore != nil {
		_ = s.agentStore.DeleteAgent(r.Context(), name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已注销"})
}

// handleSetDefaultAgent 设置默认 Agent
func (s *Server) handleSetDefaultAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := s.agentRouter.SetDefault(req.Name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if s.agentStore != nil {
		_ = s.agentStore.SetDefault(r.Context(), req.Name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "默认 Agent 已设置", "name": req.Name})
}

// --- 路由规则 API ---

// handleListRules 列出所有路由规则
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	if s.agentRouter == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"rules": []any{},
			"total": 0,
		})
		return
	}
	rules := s.agentRouter.ListRules()
	writeJSON(w, http.StatusOK, map[string]any{
		"rules": rules,
		"total": len(rules),
	})
}

// AddRuleRequest 添加路由规则
type AddRuleRequest struct {
	Platform   string `json:"platform"`
	InstanceID string `json:"instance_id"`
	UserID     string `json:"user_id"`
	ChatID     string `json:"chat_id"`
	AgentName  string `json:"agent_name"`
	Priority   int    `json:"priority"`
}

// handleAddRule 添加路由规则（内存 + 持久化）
func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var req AddRuleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	rule := router.Rule{
		Platform:   req.Platform,
		InstanceID: req.InstanceID,
		UserID:     req.UserID,
		ChatID:     req.ChatID,
		AgentName:  req.AgentName,
		Priority:   req.Priority,
	}
	if s.agentStore != nil {
		if err := s.agentStore.SaveRule(r.Context(), &rule); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + err.Error()})
			return
		}
	}
	if err := s.agentRouter.AddRule(rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "规则已添加", "id": rule.ID})
}

type TestRouteRequest struct {
	Platform   string `json:"platform"`
	InstanceID string `json:"instance_id"`
	UserID     string `json:"user_id"`
	ChatID     string `json:"chat_id"`
	Message    string `json:"message"`
}

// handleTestRoute 返回路由规则命中详情，便于前端解释“为什么这样回答”。
func (s *Server) handleTestRoute(w http.ResponseWriter, r *http.Request) {
	var req TestRouteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}

	explanation := s.agentRouter.Explain(r.Context(), router.RouteRequest{
		Platform:   req.Platform,
		InstanceID: req.InstanceID,
		UserID:     req.UserID,
		ChatID:     req.ChatID,
	}, req.Message)

	message := "未命中任何规则"
	switch explanation.Source {
	case router.RouteSourceRule:
		message = "命中显式路由规则"
	case router.RouteSourceLLM:
		message = "未命中显式规则，已通过 LLM 语义路由选择 Agent"
	case router.RouteSourceDefault:
		message = "未命中规则，已回退到默认 Agent"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"matched":    explanation.Matched,
		"agent_name": explanation.AgentName,
		"source":     explanation.Source,
		"rule":       explanation.Rule,
		"score":      explanation.Score,
		"matches":    explanation.Matches,
		"message":    message,
	})
}

// handleDeleteRule 删除单条路由规则
func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的规则 ID"})
		return
	}
	if err := s.agentRouter.RemoveRule(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if s.agentStore != nil {
		_ = s.agentStore.DeleteRule(r.Context(), id)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "规则已删除"})
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

// handleVoiceTranscribe POST /api/v1/voice/transcribe
//
// 接收音频数据（multipart/form-data 的 audio 字段或 raw body），返回转录文本。
// 限制 10MB。
func (s *Server) handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	if !s.voiceSvc.HasSTT() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "STT 服务未配置"})
		return
	}

	const maxAudioSize = 10 << 20 // 10MB
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioSize)

	var audioData []byte
	var err error

	// 支持 multipart 和 raw body 两种方式
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		file, _, fErr := r.FormFile("audio")
		if fErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 audio 文件字段"})
			return
		}
		defer file.Close()
		audioData, err = io.ReadAll(file)
	} else {
		audioData, err = io.ReadAll(r.Body)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取音频数据失败: " + err.Error()})
		return
	}

	lang := r.URL.Query().Get("language")
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "wav"
	}

	result, err := s.voiceSvc.Transcribe(r.Context(), audioData, voice.TranscribeOptions{
		Language: lang,
		Format:   voice.AudioFormat(format),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "转录失败: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleVoiceSynthesize POST /api/v1/voice/synthesize
//
// 接收文本，返回合成的音频数据。
func (s *Server) handleVoiceSynthesize(w http.ResponseWriter, r *http.Request) {
	if !s.voiceSvc.HasTTS() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TTS 服务未配置"})
		return
	}

	var req struct {
		Text  string `json:"text"`
		Voice string `json:"voice,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text 不能为空"})
		return
	}

	result, err := s.voiceSvc.Synthesize(r.Context(), req.Text, voice.SynthesizeOptions{
		Voice: req.Voice,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "合成失败: " + err.Error()})
		return
	}

	// 直接返回音频二进制
	contentType := "audio/mpeg" // 默认 mp3
	switch result.Format {
	case voice.FormatWAV:
		contentType = "audio/wav"
	case voice.FormatOGG:
		contentType = "audio/ogg"
	case voice.FormatFLAC:
		contentType = "audio/flac"
	case voice.FormatPCM:
		contentType = "audio/pcm"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Audio)
}

// syncEngineMarketplaceSkills 将磁盘上的 Markdown 技能与 ReAct 引擎注册表对齐（安装/卸载后调用）
func (s *Server) syncEngineMarketplaceSkills() {
	if s.mp == nil {
		return
	}
	e, ok := s.engine.(*engine.ReActEngine)
	if !ok {
		return
	}
	if err := e.SyncMarkdownSkillsFromMarketplace(s.mp); err != nil {
		log.Printf("技能市场: 同步引擎注册表失败: %v", err)
	}
}
