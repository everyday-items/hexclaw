package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/webhook"
)

// --- Webhook API ---

// handleListWebhooks 列出用户的 Webhook
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	if s.webhookMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"webhooks": []any{}, "total": 0})
		return
	}
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
	Secret string `json:"secret"` // 签名验证 Secret；空 = 服务端自动生成
	Prompt string `json:"prompt"` // Agent 处理指令（JobID 为空时跑此 prompt）
	JobID  string `json:"job_id"` // §13.3(1) 非空 → 事件触发指定 cron job 而非跑 prompt
	UserID string `json:"user_id"`
	// Enabled 缺省 false：创建即得端点、默认未启用——先配对端/验签/授权，
	// 再显式启用（PATCH /webhooks/{name}）。
	Enabled bool `json:"enabled"`
}

// handleRegisterWebhook 注册新 Webhook
func (s *Server) handleRegisterWebhook(w http.ResponseWriter, r *http.Request) {
	var req RegisterWebhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	// §13.3(1)：JobID 非空 → 事件触发指定 cron job，不跑 prompt，故此时 prompt 可空。
	// 仅当无 JobID（走 prompt 模式）时才强制 prompt 非空。
	if req.Name == "" || (req.JobID == "" && req.Prompt == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name 不能为空；未绑定 cron job 时 prompt 也不能为空",
		})
		return
	}

	if req.UserID == "" {
		req.UserID = "api-user"
	}

	// Secret 未提供时服务端生成：验签是外部触发的第一道门，不应默认裸奔。
	// 生成的 Secret 只在创建响应里回显一次，供用户复制到对端。
	generatedSecret := ""
	if strings.TrimSpace(req.Secret) == "" {
		generatedSecret = "whs_" + idgen.NanoID()
		req.Secret = generatedSecret
	}

	wh := &webhook.Webhook{
		Name:    req.Name,
		Type:    webhook.WebhookType(req.Type),
		Secret:  req.Secret,
		Prompt:  req.Prompt,
		JobID:   req.JobID,
		UserID:  req.UserID,
		Enabled: req.Enabled, // 缺省 false：创建即得端点、默认未启用
	}

	if s.webhookMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Webhook 未启用"})
		return
	}
	if err := s.webhookMgr.Register(r.Context(), wh); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "注册 Webhook 失败: " + err.Error(),
		})
		return
	}

	resp := map[string]any{
		"id":      wh.ID,
		"name":    wh.Name,
		"url":     fmt.Sprintf("/api/v1/webhooks/%s", wh.Name),
		"enabled": wh.Enabled,
	}
	if generatedSecret != "" {
		resp["secret"] = generatedSecret
	}
	writeJSON(w, http.StatusOK, resp)
}

// UpdateWebhookRequest 启用/停用 Webhook 请求
type UpdateWebhookRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleUpdateWebhook 更新 Webhook 启用状态（PATCH /api/v1/webhooks/{name}）。
// 授权完成后启用端点；停用即回到「验签记录、423 不派发」态。
func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Webhook 未启用"})
		return
	}
	name := r.PathValue("name")
	var req UpdateWebhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled 必填"})
		return
	}
	if err := s.webhookMgr.SetEnabled(r.Context(), name, *req.Enabled); err != nil {
		// FS-10：不存在的 name 是 404（资源不存在），不是 500（服务端故障）。
		if errors.Is(err, webhook.ErrWebhookNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Webhook 不存在: " + name})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新 Webhook 失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "enabled": *req.Enabled})
}

// handleDeleteWebhook 删除 Webhook
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Webhook 未启用"})
		return
	}
	name := r.PathValue("name")
	taskRef := ""
	if wh, ok := s.webhookMgr.Get(name); ok {
		taskRef = "webhook:" + wh.ID
	}
	if err := s.webhookMgr.Unregister(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除 Webhook 失败: " + err.Error(),
		})
		return
	}
	// 授权生命周期跟随任务：删除即回收其全部任务级授权。
	s.revokeTaskGrants(r.Context(), taskRef)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Webhook 已删除"})
}
