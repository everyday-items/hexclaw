package api

import (
	"encoding/json"
	"net/http"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/instances"
)

type UpsertInstanceRequest struct {
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Enabled  bool            `json:"enabled"`
	Config   json.RawMessage `json:"config"`
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

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	list, err := s.instanceMgr.List(r.Context())
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
	writeJSON(w, http.StatusOK, instanceResponse{
		ID:        savedInst.ID,
		Provider:  savedInst.Provider,
		Name:      savedInst.Name,
		Enabled:   savedInst.Enabled,
		Status:    savedInst.Status,
		LastError: savedInst.LastError,
		Config:    savedInst.Config,
		UpdatedAt: savedInst.UpdatedAt.Format(http.TimeFormat),
		Message:   "实例已保存",
	})
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.instanceMgr.Delete(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "实例已删除", "name": name})
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

	if hc, ok := adp.(adapter.HealthChecker); ok {
		if err := hc.Health(r.Context()); err != nil {
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
