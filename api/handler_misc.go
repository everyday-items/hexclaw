package api

import (
	"encoding/json"
	"net/http"

	"github.com/everyday-items/hexclaw/canvas"
	"github.com/everyday-items/hexclaw/engine"
	"github.com/everyday-items/hexclaw/router"
)

// --- 角色 API ---

// handleListRoles 列出可用 Agent 角色
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
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
