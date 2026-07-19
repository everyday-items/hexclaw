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
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if userID == "" {
		userID = "api-user"
	}
	// Receipt 查询复用冻结的 GET collection 路由，避免为 K12 另开第六条
	// public surface。管理面仍按 binding.created_by 做 owner 校验。
	if receiptID := strings.TrimSpace(r.URL.Query().Get("receipt_id")); receiptID != "" {
		if agentID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "查询 K12 Webhook Receipt 必须提供 agent_id"})
			return
		}
		receipt, err := s.webhookMgr.GetK12ReceiptForOwner(r.Context(), receiptID, userID, agentID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Webhook Receipt 不存在"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"receipt": receipt})
		return
	}
	if bindingName := strings.TrimSpace(r.URL.Query().Get("binding_name")); bindingName != "" {
		if agentID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "查询 K12 Webhook Receipt 历史必须提供 agent_id"})
			return
		}
		receipts, err := s.webhookMgr.ListK12ReceiptsForOwner(r.Context(), bindingName, userID, agentID, 50)
		if err != nil {
			if errors.Is(err, webhook.ErrK12BindingNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "K12 Webhook 不存在"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "获取 K12 Webhook Receipt 历史失败"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts, "total": len(receipts)})
		return
	}

	webhooks, err := s.webhookMgr.List(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取 Webhook 列表失败: " + err.Error(),
		})
		return
	}

	var k12Bindings []*webhook.K12Binding
	if agentID != "" {
		k12Bindings, err = s.webhookMgr.ListK12BindingsForAgent(r.Context(), userID, agentID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "获取 K12 Webhook 列表失败"})
			return
		}
	}
	items := make([]any, 0, len(webhooks)+len(k12Bindings))
	for _, wh := range webhooks {
		items = append(items, wh)
	}
	for _, binding := range k12Bindings {
		items = append(items, binding)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhooks":     items,
		"k12_bindings": k12Bindings,
		"total":        len(items),
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
	// K12 binding fields. Owner is frozen at creation and is never read from a
	// receiver payload. learner_id is explicit until the typed Learner table lands.
	AgentID          string                 `json:"agent_id,omitempty"`
	LearnerID        string                 `json:"learner_id,omitempty"`
	AllowedEvents    []webhook.K12EventType `json:"allowed_events,omitempty"`
	AllowedWorkflows []string               `json:"allowed_workflows,omitempty"`
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

	if webhook.WebhookType(req.Type) == webhook.TypeK12 {
		if req.Name == "" || req.AgentID == "" || req.LearnerID == "" || len(req.AllowedEvents) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "K12 Webhook 的 name / agent_id / learner_id / allowed_events 必填"})
			return
		}
		if strings.TrimSpace(req.Secret) != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "K12 Webhook Secret 必须由服务端生成"})
			return
		}
		if s.webhookMgr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Webhook 未启用"})
			return
		}
		if req.UserID == "" {
			req.UserID = "api-user"
		}
		binding, oneTimeSecret, err := s.webhookMgr.CreateK12Binding(r.Context(), webhook.K12BindingInput{
			Name: req.Name, AgentID: req.AgentID, LearnerID: req.LearnerID,
			AllowedEvents: req.AllowedEvents, AllowedWorkflows: req.AllowedWorkflows,
			CreatedBy: req.UserID, Enabled: req.Enabled,
		})
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, webhook.ErrWebhookExists) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"binding": binding, "id": binding.BindingID, "binding_id": binding.BindingID,
			"name": binding.Name, "type": webhook.TypeK12,
			"url":     fmt.Sprintf("/api/v1/webhooks/%s", binding.Name),
			"enabled": binding.Status == webhook.K12BindingEnabled,
			"secret":  oneTimeSecret,
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
		if errors.Is(err, webhook.ErrWebhookExists) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "同名 Webhook 已存在: " + wh.Name,
			})
			return
		}
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
	Enabled          *bool                   `json:"enabled"`
	RotateSecret     bool                    `json:"rotate_secret,omitempty"`
	AllowedEvents    *[]webhook.K12EventType `json:"allowed_events,omitempty"`
	AllowedWorkflows []string                `json:"allowed_workflows,omitempty"`
	// RetryReceiptID reuses the frozen PATCH item surface. It is deliberately
	// exclusive with binding configuration mutations so one request cannot
	// ambiguously change authorization and redispatch a command.
	RetryReceiptID string `json:"retry_receipt_id,omitempty"`
}

// handleUpdateWebhook 更新 Webhook 启用状态（PATCH /api/v1/webhooks/{name}）。
// 授权完成后启用端点；停用即回到「验签记录、423 不派发」态。
func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Webhook 未启用"})
		return
	}
	name := r.PathValue("name")
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	var req UpdateWebhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if _, err := s.webhookMgr.GetK12Binding(r.Context(), name); err == nil {
		binding, authErr := s.webhookMgr.GetK12BindingForOwner(r.Context(), name, userID, agentID)
		if authErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Webhook 不存在: " + name})
			return
		}
		if strings.TrimSpace(req.RetryReceiptID) != "" {
			if req.Enabled != nil || req.AllowedEvents != nil || req.RotateSecret || len(req.AllowedWorkflows) > 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "retry_receipt_id 必须作为独立操作提交"})
				return
			}
			receipt, retryErr := s.webhookMgr.RetryK12ReceiptForOwner(
				r.Context(), name, req.RetryReceiptID, userID, agentID,
			)
			if retryErr != nil {
				switch {
				case errors.Is(retryErr, webhook.ErrK12BindingNotFound):
					writeJSON(w, http.StatusNotFound, map[string]string{"error": "Webhook Receipt 不存在"})
				case errors.Is(retryErr, webhook.ErrK12ReceiptNotRetryable):
					writeJSON(w, http.StatusConflict, map[string]string{"error": retryErr.Error()})
				default:
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "重试 K12 Webhook Receipt 失败"})
				}
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"receipt": receipt})
			return
		}
		updated := binding
		var oneTimeSecret string
		if req.AllowedEvents != nil {
			updated, err = s.webhookMgr.UpdateK12BindingEventsForOwner(r.Context(), name, userID, agentID, *req.AllowedEvents, req.AllowedWorkflows)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		if req.Enabled != nil {
			updated, err = s.webhookMgr.SetK12BindingEnabledForOwner(r.Context(), name, userID, agentID, *req.Enabled)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新 K12 Webhook 失败"})
				return
			}
		}
		if req.RotateSecret {
			updated, oneTimeSecret, err = s.webhookMgr.RotateK12SecretForOwner(r.Context(), name, userID, agentID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "轮换 K12 Webhook Secret 失败"})
				return
			}
		}
		if req.Enabled == nil && req.AllowedEvents == nil && !req.RotateSecret {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled / allowed_events / rotate_secret / retry_receipt_id 至少提供一项"})
			return
		}
		resp := map[string]any{"binding": updated, "name": name, "enabled": updated.Status == webhook.K12BindingEnabled}
		if oneTimeSecret != "" {
			resp["secret"] = oneTimeSecret
		}
		writeJSON(w, http.StatusOK, resp)
		return
	} else if !errors.Is(err, webhook.ErrK12BindingNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 K12 Webhook 失败"})
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
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	taskRef := ""
	if binding, err := s.webhookMgr.GetK12Binding(r.Context(), name); err == nil {
		if _, authErr := s.webhookMgr.GetK12BindingForOwner(r.Context(), name, userID, agentID); authErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Webhook 不存在: " + name})
			return
		}
		taskRef = "webhook:" + binding.BindingID
		if err := s.webhookMgr.DeleteK12BindingForOwner(r.Context(), name, userID, agentID); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, webhook.ErrK12BindingBusy) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": "删除 K12 Webhook 失败"})
			return
		}
		s.revokeTaskGrants(r.Context(), taskRef)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Webhook 已删除"})
		return
	} else if !errors.Is(err, webhook.ErrK12BindingNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 K12 Webhook 失败"})
		return
	}
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
