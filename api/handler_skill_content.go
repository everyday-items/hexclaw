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
