package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// validSkillName rejects empty names and any path-traversal attempt so a skill
// name can never escape the marketplace directory.
func validSkillName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.Contains(name, "..")
}

// handleSkillContent returns an installed skill's raw SKILL.md for viewing.
//
// GET /api/v1/skills/{name}/content
//
// Marketplace skills are read-only here: they are owned by the marketplace and
// overwritten on update, so editing them in-app is deliberately not offered.
func (s *Server) handleSkillContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法技能名称"})
		return
	}
	sk, ok := s.mp.Get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "技能未安装"})
		return
	}
	data, err := os.ReadFile(sk.FilePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取技能内容失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name":    name,
		"path":    sk.FilePath,
		"content": string(data),
	})
}

// handleClawHubSkillContent returns a marketplace (ClawHub) skill's raw SKILL.md
// WITHOUT installing it, so the user can preview before install.
//
// GET /api/v1/clawhub/skills/{name}/content
//
// It reuses the exact source-resolution and fetch path as install (same 1 MB cap,
// same local-vs-remote guard), only skipping the disk write — so it adds no new
// fetch surface, just a read-only preview.
func (s *Server) handleClawHubSkillContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法技能名称"})
		return
	}
	if s.skillHub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ClawHub 未启用"})
		return
	}
	// 离线优先：即时 seed（磁盘缓存/内嵌种子）保证目录非空；预览原文仍需网络（下方处理）。
	s.skillHub.EnsureCatalog()
	meta, ok := s.findClawHubEntry(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "技能未找到: " + name})
		return
	}
	// MCP 条目没有可渲染的 SKILL.md，预览语义不适用。
	if strings.EqualFold(strings.TrimSpace(meta.Type), "mcp") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "MCP 条目没有 SKILL.md 可预览"})
		return
	}
	content, err := s.skillHub.Content(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "获取技能内容失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name":    name,
		"content": string(content),
	})
}
