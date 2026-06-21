package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
)

type assistantSoulResponse struct {
	SystemPrompt  string `json:"system_prompt"`  // 用户自定义人设；空=使用内置默认
	IsCustom      bool   `json:"is_custom"`      // 是否已自定义
	DefaultPrompt string `json:"default_prompt"` // 引擎内置默认人设（供预览 / 恢复默认）
}

type assistantSoulUpdateRequest struct {
	SystemPrompt string `json:"system_prompt"`
}

// handleGetAssistantSoul GET /api/v1/assistant/soul
//
// 返回默认助理(小蟹)的人设(SOUL)：自定义内容（读自 ~/.hexclaw/SOUL.md）+ 是否自定义
// + 引擎内置默认（供恢复/预览）。
func (s *Server) handleGetAssistantSoul(w http.ResponseWriter, r *http.Request) {
	soul := config.ReadSoul()
	writeJSON(w, http.StatusOK, assistantSoulResponse{
		SystemPrompt:  soul,
		IsCustom:      soul != "",
		DefaultPrompt: engine.DefaultSystemPrompt(),
	})
}

// handleUpdateAssistantSoul PUT /api/v1/assistant/soul
//
// 写入 ~/.hexclaw/SOUL.md；空 system_prompt = 删除该文件，恢复内置默认人设。
// 引擎每轮对话读取该文件，保存后下一轮即时生效（无需重启）。
func (s *Server) handleUpdateAssistantSoul(w http.ResponseWriter, r *http.Request) {
	var req assistantSoulUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}
	if err := config.WriteSoul(req.SystemPrompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "保存人设失败: " + err.Error(),
		})
		return
	}
	soul := strings.TrimSpace(req.SystemPrompt)
	writeJSON(w, http.StatusOK, assistantSoulResponse{
		SystemPrompt:  soul,
		IsCustom:      soul != "",
		DefaultPrompt: engine.DefaultSystemPrompt(),
	})
}
