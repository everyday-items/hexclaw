package api

import (
	"bytes"
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
	"github.com/hexagon-codes/hexclaw/skill/hub"
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

	if err := s.fileMem.SaveStructuredEntry(req.Content, req.Type, req.Source, "", memory.EntryMeta{}); err != nil {
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
	// 用「已配置」而非「已连接」作为列表事实源：市场一键安装后冷装尚未连上的 server 也要出现在
	// UI 列表（状态另由 /mcp/status 显示未连接），不因未即时连上而消失（修复 BUG-20260626）。
	names := s.mcpMgr.ConfiguredServerNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": names,
		"total":   len(names),
	})
}

// handleAddMCPServer 动态添加 MCP Server
// mcpAddImmediateConnectTimeout 新增 MCP Server 时「即时连接」的等待窗口。
// 暖装（npx/uvx 缓存已就绪）通常秒级连上 → 返回 connected=true；冷装首次需下载组件，
// 超过本窗口即转后台 reconnectLoop（30s 周期）继续，不再硬失败（见 Manager.AddServerBestEffort）。
const mcpAddImmediateConnectTimeout = 10 * time.Second

// addMCPServerRequest 新增 MCP Server 请求。Env 为 stdio 子进程环境变量
// （数据连接器走 MCP 的凭证注入：MySQL/Redis 等通过 env 配 MYSQL_HOST/PASSWORD 等）。
type addMCPServerRequest struct {
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Transport string            `json:"transport"`
	Endpoint  string            `json:"endpoint"`
}

func (s *Server) handleAddMCPServer(w http.ResponseWriter, r *http.Request) {
	var req addMCPServerRequest
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
		Env:       req.Env,
		Endpoint:  req.Endpoint,
		Enabled:   true,
	}

	// best-effort 注册：即时连接给一个较短窗口（暖装秒连=connected；冷装 npx/uvx 下载超时则
	// 转后台重连，不硬失败）。根治「添加数据源」首次冷装失败（详见 Manager.AddServerBestEffort）。
	ctx, cancel := context.WithTimeout(r.Context(), mcpAddImmediateConnectTimeout)
	defer cancel()

	connected, err := s.mcpMgr.AddServerBestEffort(ctx, cfg)
	if err != nil {
		// 仅不可恢复错误（name 空 / Manager 已关闭）走 400；即时连接失败属可恢复，不会到这。
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("MCP Server %q 添加失败: %v", req.Name, err),
		})
		return
	}
	// 持久化到配置文件：无论是否已连接都持久化——未连上者重启后仍由 reconnectLoop 自动拉起。
	if s.cfgWriter != nil {
		if err := s.cfgWriter.AppendMCPServer(req.Name, transport, req.Command, req.Args, req.Env, req.Endpoint); err != nil {
			logger.Error("MCP Server", "name", req.Name, "添加成功但持久化失败", err)
		}
	}
	msg := fmt.Sprintf("MCP Server %q 已添加", req.Name)
	if !connected {
		msg = fmt.Sprintf("MCP Server %q 已添加，正在后台连接（首次需下载组件）", req.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg, "connected": connected})
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

// handleRestartMCPServer 重启单个 MCP Server（M3-20260710，原型 app.html:1927 服务器行「重启」）。
// 语义见 mcp.Manager.RestartServer：新连接成功才替换，失败保留原状（404=未配置/已禁用，502=连接失败）。
func (s *Server) handleRestartMCPServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server name 不能为空"})
		return
	}
	if s.mcpMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP 未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.mcpMgr.RestartServer(ctx, name); err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "not configured") || strings.Contains(err.Error(), "disabled") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("MCP Server %q 已重启", name)})
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
	// 离线优先：即时 seed（磁盘缓存/内嵌种子）保证目录非空；实际下载 .md 仍需网络（下方各自处理）。
	s.skillHub.EnsureCatalog()
	meta, ok := s.findClawHubEntry(skillName)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "技能未找到: " + skillName,
		})
		return
	}
	metaType := strings.ToLower(strings.TrimSpace(meta.Type))
	if metaType == "mcp" {
		entry, err := hub.ValidatePinnedMCPServer(hub.MCPServerMetaFromSkill(meta))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "MCP 市场条目未通过供应链校验: " + err.Error(),
			})
			return
		}
		s.installMCPFromClawHubEntry(w, r, entry)
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

func (s *Server) findClawHubEntry(skillName string) (hub.SkillMeta, bool) {
	if s.skillHub == nil {
		return hub.SkillMeta{}, false
	}
	catalog := s.skillHub.GetCatalog()
	if catalog == nil {
		return hub.SkillMeta{}, false
	}
	for _, entry := range catalog.Skills {
		if entry.Name != skillName {
			continue
		}
		return entry, true
	}
	return hub.SkillMeta{}, false
}

func (s *Server) installMCPFromClawHubEntry(w http.ResponseWriter, r *http.Request, entry hub.ValidatedMCPServer) {
	if s.mcpMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "MCP 模块未启用，无法安装 MCP 市场条目: " + entry.Name(),
		})
		return
	}

	cfg := hexmcp.ServerConfig{
		Name:      entry.Name(),
		Transport: "stdio",
		Command:   entry.Command(),
		Args:      entry.Args(),
		Enabled:   true,
	}

	// best-effort：即时连接给较短窗口，冷装 npx/uvx 首次下载超时则转后台 reconnectLoop(30s)，不硬失败
	// （与 handleAddMCPServer 一致，修复 BUG-20260627：hub 安装路径冷装硬失败 → 装不上/点了没反应）。
	ctx, cancel := context.WithTimeout(r.Context(), mcpAddImmediateConnectTimeout)
	defer cancel()

	connected, err := s.mcpMgr.AddServerBestEffort(ctx, cfg)
	if err != nil {
		// 仅不可恢复错误（name 空 / Manager 已关闭）走 400；即时连接失败属可恢复，不会到这。
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("MCP Server %q 添加失败: %v", entry.Name(), err),
		})
		return
	}
	// 无论是否已连上都持久化——未连上者重启后仍由 reconnectLoop 自动拉起。
	if s.cfgWriter != nil {
		if err := s.cfgWriter.AppendMCPServer(entry.Name(), cfg.Transport, cfg.Command, cfg.Args, cfg.Env, cfg.Endpoint); err != nil {
			logger.Error("MCP Server", "name", entry.Name(), "添加成功但持久化失败", err)
		}
	}
	msg := "MCP 条目已从 ClawHub 安装并已连接"
	if !connected {
		msg = "MCP 条目已从 ClawHub 安装，正在后台连接（首次需下载组件）"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               entry.Name(),
		"type":               "mcp",
		"message":            msg,
		"requires_restart":   false,
		"runtime_registered": connected,
		"config_hint":        entry.ConfigHint(),
		"artifact":           entry.Artifact(),
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
	Name            string                  `json:"name"`
	DisplayName     string                  `json:"display_name"`
	Description     string                  `json:"description"`
	Model           string                  `json:"model"`
	Provider        string                  `json:"provider"`
	SystemPrompt    string                  `json:"system_prompt"`
	Skills          []string                `json:"skills"`
	MaxTokens       int                     `json:"max_tokens"`
	ReasoningPolicy *config.ReasoningPolicy `json:"reasoning_policy"`
	// Temperature 指针语义（BUG-20260703 P2-4）：缺席=未设跟随模型默认，显式 0=确定性采样。
	Temperature *float64          `json:"temperature"`
	Metadata    map[string]string `json:"metadata"`
}

// OptionalFloat 三态 patch 字段（BUG-20260703 P2-4）：字段缺席=不改（Present=false）；
// 显式 null=清除回「未设」；数值=设置。普通 *float64 无法区分「缺席」与「null」。
type OptionalFloat struct {
	Present bool
	Value   *float64
}

func (o *OptionalFloat) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(bytes.TrimSpace(data)) == "null" {
		o.Value = nil
		return nil
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	o.Value = &f
	return nil
}

type UpdateAgentRequest struct {
	DisplayName     *string                 `json:"display_name"`
	Description     *string                 `json:"description"`
	Model           *string                 `json:"model"`
	Provider        *string                 `json:"provider"`
	SystemPrompt    *string                 `json:"system_prompt"`
	Skills          *[]string               `json:"skills"`
	MaxTokens       *int                    `json:"max_tokens"`
	ReasoningPolicy *config.ReasoningPolicy `json:"reasoning_policy"`
	Temperature     OptionalFloat           `json:"temperature"`
	Metadata        *map[string]string      `json:"metadata"`
}

var k12ProfileOwnedMetadataKeys = [...]string{
	"k12.child_name",
	"k12.grade_term",
	"k12.textbook_edition",
	"k12.textbook_edition.math",
	"k12.textbook_edition.chinese",
	"k12.textbook_edition.english",
	"k12.textbook_edition.science",
	"k12.textbook_edition.information_technology",
	"k12.textbook_edition.art",
}

func k12ProfileFieldsTouched(existing *router.AgentConfig, req UpdateAgentRequest) bool {
	if existing == nil || existing.Metadata["scenario"] != "k12-tutor" {
		return false
	}
	if req.DisplayName != nil || req.Description != nil || req.SystemPrompt != nil ||
		req.Provider != nil || req.Model != nil || req.Skills != nil {
		return true
	}
	if req.Metadata == nil {
		return false
	}
	next := *req.Metadata
	for _, key := range k12ProfileOwnedMetadataKeys {
		before, beforeOK := existing.Metadata[key]
		after, afterOK := next[key]
		if beforeOK != afterOK || before != after {
			return true
		}
	}
	return false
}

// validateAgentTemperature 温度合法域 [0,2]（nil=未设不校验）。
func validateAgentTemperature(t *float64) error {
	if t != nil && (*t < 0 || *t > 2) {
		return fmt.Errorf("temperature 必须在 [0,2] 区间")
	}
	return nil
}

func normalizeAPIReasoningPolicy(policy **config.ReasoningPolicy) error {
	if policy == nil {
		return fmt.Errorf("reasoning_policy destination is nil")
	}
	if *policy == nil {
		inherit := config.ReasoningPolicy{Mode: config.ReasoningPolicyModeInherit}
		*policy = &inherit
		return nil
	}
	candidate := **policy
	if err := candidate.Validate(true); err != nil {
		return err
	}
	*policy = &candidate
	return nil
}

// validateAgentMetadataCapabilities applies the scenario-owned capability
// guard before generic Agent state reaches the router or persistent store.
func (s *Server) validateAgentMetadataCapabilities(metadata map[string]string) error {
	if metadata == nil || s.agentMetadataGuard == nil {
		return nil
	}
	return s.agentMetadataGuard(metadata)
}

func cloneLLMConfigSnapshot(source config.LLMConfig) config.LLMConfig {
	clone := source
	clone.Providers = make(map[string]config.LLMProviderConfig, len(source.Providers))
	for name, provider := range source.Providers {
		providerClone := provider
		if provider.Models != nil {
			providerClone.Models = append([]string{}, provider.Models...)
		}
		if provider.ModelSpecs != nil {
			providerClone.ModelSpecs = make([]config.LLMProviderModelSpec, len(provider.ModelSpecs))
			for index, spec := range provider.ModelSpecs {
				specClone := spec
				if spec.Capabilities != nil {
					specClone.Capabilities = append([]string{}, spec.Capabilities...)
				}
				if spec.Embedding != nil {
					embedding := *spec.Embedding
					specClone.Embedding = &embedding
				}
				providerClone.ModelSpecs[index] = specClone
			}
		}
		if provider.ToolsEnabled != nil {
			toolsEnabled := *provider.ToolsEnabled
			providerClone.ToolsEnabled = &toolsEnabled
		}
		if provider.Enabled != nil {
			enabled := *provider.Enabled
			providerClone.Enabled = &enabled
		}
		clone.Providers[name] = providerClone
	}
	return clone
}

// persistedLLMConfig returns the complete control-plane configuration, including
// disabled or temporarily unloadable providers. Configuration GETs and stable
// provider-identity lookups must use this snapshot rather than the routing
// runtime, whose active set intentionally filters those providers.
func (s *Server) persistedLLMConfig() config.LLMConfig {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return cloneLLMConfigSnapshot(s.cfg.LLM)
}

func (s *Server) activeLLMConfig() config.LLMConfig {
	// cfgMu also covers runtime ReloadLLMConfig in the PUT path. Reading both
	// sources and cloning every mutable map/slice/pointer while holding the same
	// lock yields one immutable generation and prevents catalog probes from
	// racing a concurrent config transition.
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	llmCfg := cloneLLMConfigSnapshot(s.cfg.LLM)
	if runtime, ok := s.engine.(llmConfigRuntime); ok {
		live := runtime.ActiveLLMConfig()
		if len(live.Providers) > 0 {
			llmCfg = cloneLLMConfigSnapshot(live)
		}
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
	// 禁用的 provider（Enabled=false）不参与路由，绑定到它的 Agent 运行期必失败 →
	// 注册时直接拒绝，给出清晰错误而非留到调用期才暴底层错误（BUG-20260625 §3-2）。
	if p := llmCfg.Providers[providerKey]; p.Enabled != nil && !*p.Enabled {
		return fmt.Errorf("指定的 provider %q 已禁用，请先在设置中启用", cfg.Provider)
	}
	if err := validateConfiguredTextModel(llmCfg, providerKey, cfg.Model); err != nil {
		return err
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
		Name:            req.Name,
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		Model:           req.Model,
		Provider:        req.Provider,
		SystemPrompt:    req.SystemPrompt,
		Skills:          req.Skills,
		MaxTokens:       req.MaxTokens,
		ReasoningPolicy: req.ReasoningPolicy,
		Temperature:     req.Temperature,
		Metadata:        req.Metadata,
	}
	if err := normalizeAPIReasoningPolicy(&cfg.ReasoningPolicy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := s.validateAgentMetadataCapabilities(cfg.Metadata); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.validateAgentLLMConfig(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateAgentTemperature(cfg.Temperature); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var persistErr error
	err := s.agentRouter.RegisterPersisted(cfg, func(candidate *router.AgentConfig) error {
		if s.agentStore == nil {
			return nil
		}
		persistErr = s.agentStore.SaveAgent(r.Context(), candidate)
		return persistErr
	})
	if err != nil {
		if persistErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + persistErr.Error()})
		} else {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		}
		return
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
	if k12ProfileFieldsTouched(existing, req) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "K12 profile fields require /api/k12/profile-bundle",
		})
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
	if req.ReasoningPolicy != nil {
		cfg.ReasoningPolicy = req.ReasoningPolicy
		if err := normalizeAPIReasoningPolicy(&cfg.ReasoningPolicy); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Temperature.Present {
		// 三态：null=清除回「未设」（Value=nil），数值=设置（BUG-20260703 P2-4）
		if err := validateAgentTemperature(req.Temperature.Value); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg.Temperature = req.Temperature.Value
	}
	if req.Metadata != nil {
		cfg.Metadata = *req.Metadata
	}
	if err := s.validateAgentMetadataCapabilities(cfg.Metadata); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// BUG-20260703 D3：LLM 配置校验只跟着真改动走——请求未碰（或原样回传）model/provider
	// 时不重审存量值，否则 provider 失效后连 display_name/system_prompt 都被连坐锁死。
	// 真改动时校验保持不放松（禁用 provider 拒绝语义见 BUG-20260625 §3-2）。
	llmChanged := (req.Model != nil && strings.TrimSpace(*req.Model) != existing.Model) ||
		(req.Provider != nil && strings.TrimSpace(*req.Provider) != existing.Provider)
	if llmChanged {
		if err := s.validateAgentLLMConfig(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		cfg.Model = existing.Model
		cfg.Provider = existing.Provider
	}
	var persistErr error
	err := s.agentRouter.UpdateAgentPersisted(name, func(current router.AgentConfig) (router.AgentConfig, error) {
		// Reapply only request-present fields to the value read under the
		// dispatcher lock. This prevents a concurrent K12 profile restore from
		// being overwritten by the stale pre-validation snapshot above.
		if req.DisplayName != nil {
			current.DisplayName = cfg.DisplayName
		}
		if req.Description != nil {
			current.Description = cfg.Description
		}
		if req.SystemPrompt != nil {
			current.SystemPrompt = cfg.SystemPrompt
		}
		if req.Skills != nil {
			current.Skills = cfg.Skills
		}
		if req.MaxTokens != nil {
			current.MaxTokens = cfg.MaxTokens
		}
		if req.ReasoningPolicy != nil {
			current.ReasoningPolicy = cfg.ReasoningPolicy
		}
		if req.Temperature.Present {
			current.Temperature = cfg.Temperature
		}
		if req.Metadata != nil {
			current.Metadata = cfg.Metadata
		}
		if llmChanged {
			current.Model = cfg.Model
			current.Provider = cfg.Provider
		}
		return current, nil
	}, func(updated *router.AgentConfig) error {
		if s.agentStore == nil {
			return nil
		}
		persistErr = s.agentStore.SaveAgent(r.Context(), updated)
		return persistErr
	})
	if err != nil {
		if persistErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + persistErr.Error()})
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Agent 已更新", "name": name})
}

// handleUnregisterAgent 注销 Agent（内存 + 持久化）
func (s *Server) handleUnregisterAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agent, exists := s.agentRouter.GetAgent(name)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("agent %q 未注册", name)})
		return
	}

	var detachedResources AgentResourceDetach
	var persistErr error
	var resourceErr error
	err := s.agentRouter.UnregisterPersisted(name, func(name, nextDefault string, wasDefault bool) error {
		// UnregisterPersisted holds the dispatcher write lock across this
		// callback. Staging owned-resource cleanup here closes the provision vs
		// delete race: a provisioner that validates through the same dispatcher
		// cannot recreate schedules between cleanup and Agent removal.
		if s.agentResources != nil {
			detachedResources, resourceErr = s.agentResources.DetachAgentResources(r.Context(), *agent)
			if resourceErr != nil {
				return fmt.Errorf("清理 Agent 归属资源失败: %w", resourceErr)
			}
		}
		if s.agentStore == nil {
			return nil
		}
		if atomicStore, ok := s.agentStore.(router.AtomicAgentUnregisterStore); ok {
			persistErr = atomicStore.DeleteAgentAndSetDefault(r.Context(), name, nextDefault, wasDefault)
			return persistErr
		}
		// A non-default deletion is one durable mutation and remains atomic through
		// Store.DeleteAgent. Deleting the default requires reassignment in the same
		// transaction; fail closed if this Store cannot provide that boundary.
		if wasDefault {
			persistErr = fmt.Errorf("agent store 不支持默认 Agent 原子注销")
			return persistErr
		}
		persistErr = s.agentStore.DeleteAgent(r.Context(), name)
		return persistErr
	})
	if err != nil {
		if detachedResources.Rollback != nil {
			// The request can already be canceled by the time persistence
			// reports an error. Compensation must still get a chance to restore
			// the resources staged above.
			if rollbackErr := detachedResources.Rollback(context.WithoutCancel(r.Context())); rollbackErr != nil {
				logger.Error("Agent 注销资源回滚失败", "agent", name, "error", rollbackErr)
				err = fmt.Errorf("%w; 归属资源回滚失败: %v", err, rollbackErr)
			}
		}
		if resourceErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": resourceErr.Error()})
		} else if persistErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + persistErr.Error()})
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return
	}
	if detachedResources.Commit != nil {
		detachedResources.Commit()
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
	var persistErr error
	err := s.agentRouter.SetDefaultPersisted(req.Name, func(name string) error {
		if s.agentStore == nil {
			return nil
		}
		persistErr = s.agentStore.SetDefault(r.Context(), name)
		return persistErr
	})
	if err != nil {
		if persistErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + persistErr.Error()})
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return
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
	var persistErr error
	err := s.agentRouter.RemoveRulePersisted(id, func(id int) error {
		if s.agentStore == nil {
			return nil
		}
		persistErr = s.agentStore.DeleteRule(r.Context(), id)
		return persistErr
	})
	if err != nil {
		if persistErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "规则持久化删除失败: " + persistErr.Error()})
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return
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
	// 纯空白也算空：与 Service 层 TrimSpace 判定一致，避免空白文本绕到 Service 报错被包成 500。
	if strings.TrimSpace(req.Text) == "" {
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

// ─── MCP 功能优先校验 ────────────────────────────────────

// MCP stdio 子进程通过 exec(argv) 启动，不经过 shell。默认不做命令白名单或
// shell 元字符拦截，避免挡住自定义 MCP server、JSON 参数、连接串、路径等合法配置；
// 仅拒绝空命令和控制字符。
const mcpControlChars = "\n\r\x00"

func validateMCPCommand(command string, args []string) error {
	if command == "" {
		return fmt.Errorf("command 不能为空")
	}
	if strings.ContainsAny(command, mcpControlChars) {
		return fmt.Errorf("command 包含控制字符")
	}
	for i, arg := range args {
		if strings.ContainsAny(arg, mcpControlChars) {
			return fmt.Errorf("args[%d] 包含控制字符", i)
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
