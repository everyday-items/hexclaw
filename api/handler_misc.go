package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/media/voice"
	"github.com/hexagon-codes/hexclaw/canvas"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/httpua"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	"github.com/hexagon-codes/toolkit/util/logger"
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
	Icon             string   `json:"icon,omitempty"`
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

// handleGetMemory 获取结构化记忆列表
func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if rawLimit := q.Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit 参数无效"})
			return
		}
		limit = n
	}

	result, err := s.fileMem.ListEntries(memory.ListOptions{
		View:   q.Get("view"),
		Limit:  limit,
		Cursor: q.Get("cursor"),
		Type:   q.Get("type"),
		Source: q.Get("source"),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     result.Entries,
		"summary":     s.fileMem.LoadContext(),
		"capacity":    s.fileMem.Capacity(),
		"total":       result.Total,
		"next_cursor": result.NextCursor,
		"has_more":    result.HasMore,
	})
}

// SaveMemoryRequest 保存记忆请求
type SaveMemoryRequest struct {
	Content string `json:"content"` // 记忆内容
	Type    string `json:"type"`    // identity/preference/fact/instruction/context
	Source  string `json:"source"`  // manual/chat_explicit/chat_extract/system
}

// handleSaveMemory 创建单条记忆
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

	if err := s.fileMem.SaveEntry(req.Content, req.Type, req.Source); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "保存记忆失败: " + err.Error(),
		})
		return
	}

	// 返回刚创建的条目（取最后一条）
	entries := s.fileMem.ParseEntries()
	if len(entries) > 0 {
		writeJSON(w, http.StatusOK, entries[len(entries)-1])
	} else {
		writeJSON(w, http.StatusOK, map[string]string{"message": "记忆已保存"})
	}
}

// handleSearchMemory 搜索记忆 (FileMemory 关键词 + VectorMemory 语义)
func (s *Server) handleSearchMemory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "q 参数不能为空",
		})
		return
	}

	// 1. FileMemory 关键词搜索
	fileResults := s.fileMem.Search(query)

	// 2. VectorMemory 语义搜索 (D7: 链路④ 记忆闭环)
	type vectorResult struct {
		Content string  `json:"content"`
		Score   float32 `json:"score"`
		Source  string  `json:"source"`
	}
	var vecResults []vectorResult
	if s.vectorMem != nil {
		if vr, err := s.vectorMem.Search(r.Context(), query, 5); err == nil {
			for _, r := range vr {
				vecResults = append(vecResults, vectorResult{
					Content: r.Content,
					Score:   r.Score,
					Source:  "vector",
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"results":        fileResults,
		"vector_results": vecResults,
		"total":          len(fileResults) + len(vecResults),
	})
}

// --- MCP API ---

// handleListMCPTools 列出所有已发现的 MCP 工具
func (s *Server) handleListMCPTools(w http.ResponseWriter, r *http.Request) {
	if s.mcpMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tools": []any{}, "total": 0})
		return
	}
	infos := s.mcpMgr.ToolInfos()
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": infos,
		"total": len(infos),
	})
}

// handleListMCPServers 列出已连接的 MCP Server
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	if s.mcpMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []any{}, "total": 0})
		return
	}
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

	// 安全校验：stdio command 必须是已知安全的可执行文件，禁止 shell 元字符
	if transport == "stdio" {
		if err := validateMCPCommand(req.Command, req.Args); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	// 安全校验：sse/streamable endpoint 必须是合法 URL
	if (transport == "sse" || transport == "streamable") && req.Endpoint != "" {
		if err := validateMCPEndpoint(req.Endpoint); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	if s.mcpMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP 未启用"})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("MCP Server %q 连接失败: %v", req.Name, err),
		})
		return
	}
	// 持久化到配置文件，重启后不丢失
	if s.cfgWriter != nil {
		if err := s.cfgWriter.AppendMCPServer(req.Name, transport, req.Command, req.Args, req.Endpoint); err != nil {
			logger.Error("MCP Server", "name", req.Name, "添加成功但持久化失败", err)
		}
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

	if s.mcpMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP 未启用"})
		return
	}
	if err := s.mcpMgr.RemoveServer(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// 从配置文件中移除
	if s.cfgWriter != nil {
		_ = s.cfgWriter.RemoveMCPServer(name)
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
			Icon:             sk.Meta.Icon,
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
	Source  string `json:"source"`            // 源路径 / URL / clawhub 技能名（type=content 时可空，改用 content）
	Type    string `json:"type,omitempty"`    // "file" | "url" | "clawhub" | "content"（缺省时自动推断）
	Content string `json:"content,omitempty"` // type=content 时：完整 SKILL.md 原文（AI 生成/就地编辑后落盘）
}

// handleInstallSkill 安装技能
//
// type 支持三种来源:
//   - "clawhub" 或 source 前缀 clawhub:// — 从 ClawHub 在线安装
//   - "file"                              — 从本地文件系统安装（允许绝对路径，供桌面端文件选择器使用）
//   - "url"                               — 从 HTTPS URL 下载后安装
//
// type 为空时按 source 前缀自动推断（向后兼容）。
func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var req InstallSkillRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	// type=content 改用 content 字段承载原文；其余类型要求 source 非空。
	if req.Type != "content" && req.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "source 不能为空",
		})
		return
	}

	// 自动推断 type（向后兼容无 type 字段的旧请求）
	if req.Type == "" {
		switch {
		case strings.HasPrefix(req.Source, "clawhub://"):
			req.Type = "clawhub"
			req.Source = strings.TrimPrefix(req.Source, "clawhub://")
		case strings.HasPrefix(req.Source, "https://"):
			req.Type = "url"
		default:
			req.Type = "file"
		}
	}

	switch req.Type {
	case "clawhub":
		s.installSkillFromClawHub(w, r, strings.TrimPrefix(req.Source, "clawhub://"))
	case "file":
		s.installSkillFromFile(w, req.Source)
	case "url":
		s.installSkillFromURL(w, r, req.Source)
	case "content":
		s.installSkillFromContent(w, req.Content)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的安装类型: " + req.Type,
		})
	}
}

var (
	// frontmatter 内提取 name 的标量行（限 frontmatter 段内调用）。
	skillNameLineRe = regexp.MustCompile(`(?m)^name:\s*["']?([A-Za-z0-9_-]+)["']?\s*$`)
	// 合法 skill 标识：小写字母/数字/连字符/下划线（比 validSkillName 的路径安全检查更严，
	// 用于 content 安装时强校验 frontmatter name，避免空 name 回退临时文件名）。
	validSkillIdentifierRe = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// frontmatterSkillName 从 SKILL.md 首段 frontmatter（首个 --- 与闭合 --- 之间）提取 name。
// 必须有闭合 frontmatter 才在其中查找，避免把正文里恰好出现的 `name:` 行误当 skill 名。
func frontmatterSkillName(content string) string {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "---") {
		return ""
	}
	rest := strings.TrimPrefix(s, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "" // frontmatter 未闭合
	}
	m := skillNameLineRe.FindStringSubmatch(rest[:end])
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func validSkillIdentifier(name string) bool {
	return name != "" && validSkillIdentifierRe.MatchString(name)
}

// installSkillFromContent 把一份完整 SKILL.md 原文写盘安装（AI 生成 / 就地编辑后落盘）。
// 复用 URL 安装的「临时文件 → mp.Install」路径，校验 frontmatter 标记、name 合法性与大小上限。
func (s *Server) installSkillFromContent(w http.ResponseWriter, content string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content 不能为空"})
		return
	}
	if len(content) > skillURLMaxSize {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "内容过大（上限 1 MB）"})
		return
	}
	if !strings.HasPrefix(trimmed, "---") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "内容不是有效的 Skill 格式（缺少 frontmatter）",
		})
		return
	}
	// content 来源没有「真实文件名」语义：必须自带合法 frontmatter name，否则 mp.Install 会
	// 回退用临时文件名（hexclaw-skill-*.md）当 skill 名落盘（bug fix 2026-06-22 review）。
	if name := frontmatterSkillName(trimmed); !validSkillIdentifier(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "SKILL.md 缺少合法的 name 字段（要求小写字母/数字/连字符/下划线）",
		})
		return
	}

	tmpFile, err := os.CreateTemp("", "hexclaw-skill-*.md")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建临时文件失败: " + err.Error()})
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入临时文件失败: " + err.Error()})
		return
	}
	tmpFile.Close()

	sk, err := s.mp.Install(tmpPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "安装技能失败: " + err.Error()})
		return
	}

	s.syncEngineMarketplaceSkills()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               sk.Meta.Name,
		"description":        sk.Meta.Description,
		"version":            sk.Meta.Version,
		"message":            "技能已创建并同步到运行引擎",
		"requires_restart":   false,
		"runtime_registered": true,
	})
}

// installSkillFromClawHub 从 ClawHub 在线安装
func (s *Server) installSkillFromClawHub(w http.ResponseWriter, r *http.Request, skillName string) {
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
	meta, ok := s.findClawHubEntry(skillName)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "技能未找到: " + skillName,
		})
		return
	}
	metaType := strings.ToLower(strings.TrimSpace(meta.Type))
	if metaType == "mcp" {
		s.installMCPFromClawHubEntry(w, r, skillName, meta.Command, meta.Args, meta.ConfigHint)
		return
	}
	if err := s.skillHub.Install(r.Context(), skillName); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "安装技能失败: " + err.Error(),
		})
		return
	}
	_ = s.mp.Init()
	s.syncEngineMarketplaceSkills()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               skillName,
		"message":            "技能已从 ClawHub 安装并已同步到运行引擎",
		"requires_restart":   false,
		"runtime_registered": true,
	})
}

func (s *Server) findClawHubEntry(skillName string) (meta struct {
	Type       string
	Command    string
	Args       []string
	ConfigHint string
}, ok bool) {
	if s.skillHub == nil {
		return meta, false
	}
	catalog := s.skillHub.GetCatalog()
	if catalog == nil {
		return meta, false
	}
	for _, entry := range catalog.Skills {
		if entry.Name != skillName {
			continue
		}
		meta.Type = entry.Type
		meta.Command = entry.Command
		meta.Args = entry.Args
		meta.ConfigHint = entry.ConfigHint
		return meta, true
	}
	return meta, false
}

func (s *Server) installMCPFromClawHubEntry(w http.ResponseWriter, r *http.Request, name, command string, args []string, configHint string) {
	if s.mcpMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "MCP 模块未启用，无法安装 MCP 市场条目: " + name,
		})
		return
	}
	if err := validateMCPCommand(command, args); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "MCP 市场条目配置无效: " + err.Error(),
		})
		return
	}

	cfg := hexmcp.ServerConfig{
		Name:      name,
		Transport: "stdio",
		Command:   command,
		Args:      args,
		Enabled:   true,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := s.mcpMgr.AddServer(ctx, cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("MCP Server %q 连接失败: %v", name, err),
		})
		return
	}
	if s.cfgWriter != nil {
		if err := s.cfgWriter.AppendMCPServer(name, cfg.Transport, cfg.Command, cfg.Args, cfg.Endpoint); err != nil {
			logger.Error("MCP Server", "name", name, "添加成功但持久化失败", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               name,
		"type":               "mcp",
		"message":            "MCP 条目已从 ClawHub 安装并已连接",
		"requires_restart":   false,
		"runtime_registered": true,
		"config_hint":        configHint,
	})
}

// installSkillFromFile 从本地文件安装
//
// 允许绝对路径（桌面端文件选择器返回绝对路径）。
// 安全校验：后缀 .md、无路径穿越、文件大小 < 1 MB。
func (s *Server) installSkillFromFile(w http.ResponseWriter, source string) {
	// 路径穿越检查
	if strings.Contains(source, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "source 路径不安全",
		})
		return
	}

	info, err := os.Stat(source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文件不存在: " + err.Error(),
		})
		return
	}

	// 单文件校验
	if !info.IsDir() {
		if !strings.HasSuffix(strings.ToLower(source), ".md") {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "仅支持 .md 技能文件",
			})
			return
		}
		if info.Size() > 1<<20 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "文件过大（上限 1 MB）",
			})
			return
		}
	}

	sk, err := s.mp.Install(source)
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

const skillURLMaxSize = 1 << 20 // 1 MB

// installSkillFromURL 从 HTTPS URL 下载后安装
func (s *Server) installSkillFromURL(w http.ResponseWriter, r *http.Request, rawURL string) {
	if !strings.HasPrefix(rawURL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "仅支持 HTTPS URL",
		})
		return
	}

	// 校验 URL 格式
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "URL 格式无效",
		})
		return
	}

	// 下载
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "创建请求失败: " + err.Error(),
		})
		return
	}
	httpua.Set(req) // 默认浏览器 UA，避免反爬站对 Go 默认 UA 返回 HTML（AP-016）

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "下载失败: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode),
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, skillURLMaxSize+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "读取响应失败: " + err.Error(),
		})
		return
	}
	if len(body) > skillURLMaxSize {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "远程文件过大（上限 1 MB）",
		})
		return
	}

	// 基本内容校验：必须包含 frontmatter 标记
	content := string(body)
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文件内容不是有效的 Skill 格式（缺少 frontmatter）",
		})
		return
	}

	// 写入临时文件
	tmpFile, err := os.CreateTemp("", "hexclaw-skill-*.md")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "创建临时文件失败: " + err.Error(),
		})
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(body); err != nil {
		tmpFile.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "写入临时文件失败: " + err.Error(),
		})
		return
	}
	tmpFile.Close()

	sk, err := s.mp.Install(tmpPath)
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
		"message":            "技能已从远程 URL 安装并已同步到运行引擎",
		"requires_restart":   false,
		"runtime_registered": true,
	})
}

// handleUninstallSkill 删除技能
func (s *Server) handleUninstallSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mp.Uninstall(name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, marketplace.ErrSkillNotInstalled) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{
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

func (s *Server) activeLLMConfig() config.LLMConfig {
	llmCfg := s.cfg.LLM
	if runtime, ok := s.engine.(llmConfigRuntime); ok {
		llmCfg = effectiveLLMConfig(llmCfg, runtime)
	}
	return llmCfg
}

func findLLMProviderKey(llmCfg config.LLMConfig, provider string) (string, bool) {
	trimmedProvider := strings.TrimSpace(provider)
	if trimmedProvider == "" {
		return "", false
	}
	if _, ok := llmCfg.Providers[trimmedProvider]; ok {
		return trimmedProvider, true
	}

	normalizedProvider := strings.ToLower(trimmedProvider)
	for name := range llmCfg.Providers {
		if strings.ToLower(strings.TrimSpace(name)) == normalizedProvider {
			return name, true
		}
	}

	return "", false
}

func (s *Server) validateAgentLLMConfig(cfg *router.AgentConfig) error {
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	cfg.Model = strings.TrimSpace(cfg.Model)

	if cfg.Provider == "" && cfg.Model == "" {
		return nil
	}
	if cfg.Provider == "" || cfg.Model == "" {
		return fmt.Errorf("provider 和 model 必须同时指定")
	}

	llmCfg := s.activeLLMConfig()
	providerKey, ok := findLLMProviderKey(llmCfg, cfg.Provider)
	if !ok {
		return fmt.Errorf("指定的 provider %q 不存在", cfg.Provider)
	}
	cfg.Provider = providerKey
	return nil
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

	if err := s.validateAgentLLMConfig(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
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
	if err := s.validateAgentLLMConfig(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
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
	msg := "默认 Agent 已设置"
	if req.Name == "" {
		msg = "默认 Agent 已清除"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "name": req.Name})
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
	if req.AgentName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name 不能为空"})
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
		logger.Error("技能市场: 同步引擎注册表失败", "error", err)
	}
}

// ─── MCP 安全校验 ────────────────────────────────────

// mcpAllowedCommands stdio 模式允许的命令白名单
var mcpAllowedCommands = map[string]bool{
	"npx": true, "node": true, "uvx": true, "uv": true, "python": true, "python3": true,
	"docker": true, "deno": true, "bun": true, "go": true, "cargo": true,
}

// mcpDangerousChars shell 元字符 + 控制字符，禁止出现在 command/args 中
const mcpDangerousChars = "`$|;&><(){}!\\'\"~\n\r\x00"

func validateMCPCommand(command string, args []string) error {
	if command == "" {
		return fmt.Errorf("command 不能为空")
	}
	// 解析 symlink 防止伪装绕过白名单
	resolved := command
	if filepath.IsAbs(command) {
		if real, err := filepath.EvalSymlinks(command); err == nil {
			resolved = real
		}
	}
	base := filepath.Base(resolved)
	if !mcpAllowedCommands[base] {
		return fmt.Errorf("不允许的命令 %q，仅支持: npx, node, uvx, uv, python, python3, docker, deno, bun, go, cargo", base)
	}
	// 禁止 shell 元字符
	if strings.ContainsAny(command, mcpDangerousChars) {
		return fmt.Errorf("command 包含不允许的字符")
	}
	for i, arg := range args {
		if strings.ContainsAny(arg, mcpDangerousChars) {
			return fmt.Errorf("args[%d] 包含不允许的字符", i)
		}
	}
	return nil
}

func validateMCPEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint URL 格式错误: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint 仅支持 http/https 协议，收到: %s", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint 缺少 host")
	}
	return nil
}
