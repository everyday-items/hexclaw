package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/everyday-items/hexclaw/webhook"
)

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
