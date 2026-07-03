package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/instances"
)

type UpsertInstanceRequest struct {
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Enabled  bool            `json:"enabled"`
	Config   json.RawMessage `json:"config"`
}

type sendTestRequest struct {
	Target  string `json:"target"`
	Content string `json:"content"`
}

type instanceResponse struct {
	ID        string           `json:"id"`
	Provider  string           `json:"provider"`
	Name      string           `json:"name"`
	Enabled   bool             `json:"enabled"`
	Status    instances.Status `json:"status"`
	LastError string           `json:"last_error,omitempty"`
	Config    json.RawMessage  `json:"config,omitempty"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	Message   string           `json:"message,omitempty"`
}

func instanceToResponse(inst *instances.Instance, message string) instanceResponse {
	return instanceResponse{
		ID:        inst.ID,
		Provider:  inst.Provider,
		Name:      inst.Name,
		Enabled:   inst.Enabled,
		Status:    inst.Status,
		LastError: inst.LastError,
		Config:    inst.Config,
		UpdatedAt: inst.UpdatedAt.Format(http.TimeFormat),
		Message:   message,
	}
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	// ListLive (not List): reconcile each started adapter's status against its
	// live connection health so the channel badge agrees with the test button
	// (BUG-20260627: "已连接" badge vs "Stream 未连接" test on the same channel).
	list, err := s.instanceMgr.ListLive(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": list,
		"total":     len(list),
	})
}

func (s *Server) handleUpsertInstance(w http.ResponseWriter, r *http.Request) {
	var req UpsertInstanceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if name := r.PathValue("name"); name != "" {
		req.Name = name
	}
	inst := &instances.Instance{
		Provider: req.Provider,
		Name:     req.Name,
		Enabled:  req.Enabled,
		Config:   req.Config,
	}
	if err := s.instanceMgr.Upsert(r.Context(), inst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Enabled {
		_ = s.instanceMgr.Stop(r.Context(), req.Name)
		if err := s.instanceMgr.Start(r.Context(), req.Name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	} else {
		_ = s.instanceMgr.Stop(r.Context(), req.Name)
	}
	savedInst, err := s.instanceMgr.Get(r.Context(), req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, instanceToResponse(savedInst, "实例已保存"))
}

func (s *Server) handleUpdateInstanceByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}
	current, err := s.instanceMgr.GetByID(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	var req UpsertInstanceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Provider) == "" {
		req.Provider = current.Provider
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = current.Name
	}
	if len(req.Config) == 0 {
		req.Config = current.Config
	}

	_ = s.instanceMgr.Stop(r.Context(), current.Name)
	next := &instances.Instance{
		ID:       current.ID,
		Provider: req.Provider,
		Name:     strings.TrimSpace(req.Name),
		Enabled:  req.Enabled,
		Config:   req.Config,
	}
	if err := s.instanceMgr.UpdateByID(r.Context(), id, next); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if req.Enabled {
		if err := s.instanceMgr.Start(r.Context(), next.Name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	savedInst, err := s.instanceMgr.GetByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, instanceToResponse(savedInst, "实例已保存"))
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.instanceMgr.Delete(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "实例已删除", "name": name})
}

func (s *Server) handleDeleteInstanceByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}
	if err := s.instanceMgr.DeleteByID(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "实例已删除", "id": id})
}

func (s *Server) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.instanceMgr.Start(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "实例已启动", "name": name})
}

func (s *Server) handleListInstanceHealth(w http.ResponseWriter, r *http.Request) {
	list, err := s.instanceMgr.HealthAll(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": list,
		"total":     len(list),
	})
}

func (s *Server) handleGetInstanceHealth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	report, err := s.instanceMgr.Health(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleStopInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.instanceMgr.Stop(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "实例已停止", "name": name})
}

func (s *Server) handleTestInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	report, err := s.instanceMgr.Health(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	message := "实例健康检查未通过"
	if report.Healthy {
		message = "实例健康检查通过"
	} else if report.LastError != "" {
		message = report.LastError
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    report.Healthy,
		"message":    message,
		"name":       report.Name,
		"provider":   report.Provider,
		"status":     report.Status,
		"last_error": report.LastError,
	})
}

func (s *Server) handleTestInstanceByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.instanceMgr.GetByID(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	r.SetPathValue("name", inst.Name)
	s.handleTestInstance(w, r)
}

func (s *Server) handleSendTestInstanceByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.instanceMgr.GetByID(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if !inst.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "实例未启用，不能发送测试消息",
		})
		return
	}

	var req sendTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	req.Content = strings.TrimSpace(req.Content)
	if req.Target == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "target 和 content 不能为空",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.instanceMgr.Start(ctx, inst.Name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "实例启动失败: " + err.Error(),
		})
		return
	}
	if err := s.instanceMgr.Send(ctx, inst.ID, req.Target, &adapter.Reply{Content: req.Content}); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"message": "测试消息发送失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "测试消息已发送",
		"id":      inst.ID,
		"name":    inst.Name,
	})
}

func (s *Server) handleTestChannelConfig(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider 不能为空"})
		return
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "配置不能为空"})
		return
	}

	inst := &instances.Instance{
		Provider: provider,
		Name:     "__test__",
		Config:   raw,
	}
	adp, err := instances.BuildAdapter(inst)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  false,
			"provider": provider,
			"message":  "配置解析失败: " + err.Error(),
		})
		return
	}

	// Only use ConfigValidator for pre-save testing. Never call Health()
	// on a freshly built adapter because it hasn't been Start()-ed
	// (handler is nil, connections not established, etc.).
	if cv, ok := adp.(adapter.ConfigValidator); ok {
		if err := cv.ValidateConfig(r.Context()); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success":  false,
				"provider": provider,
				"message":  err.Error(),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"provider": provider,
		"message":  "配置校验通过",
	})
}

func (s *Server) handlePlatformHook(w http.ResponseWriter, r *http.Request) {
	s.instanceMgr.HandleWebhook(w, r)
}
