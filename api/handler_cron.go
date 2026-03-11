package api

import (
	"encoding/json"
	"net/http"

	"github.com/everyday-items/hexclaw/cron"
)

// --- Cron API ---

// AddCronJobRequest 添加定时任务请求
type AddCronJobRequest struct {
	Name     string `json:"name"`     // 任务名称
	Schedule string `json:"schedule"` // cron 表达式或 @every/@daily 等
	Prompt   string `json:"prompt"`   // Agent 处理指令
	UserID   string `json:"user_id"`
	Type     string `json:"type"`     // cron 或 once
}

// handleListCronJobs 列出定时任务
func (s *Server) handleListCronJobs(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}

	jobs, err := s.scheduler.ListJobs(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取任务列表失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":  jobs,
		"total": len(jobs),
	})
}

// handleAddCronJob 添加定时任务
func (s *Server) handleAddCronJob(w http.ResponseWriter, r *http.Request) {
	var req AddCronJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Name == "" || req.Schedule == "" || req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name、schedule 和 prompt 不能为空",
		})
		return
	}

	if req.UserID == "" {
		req.UserID = "api-user"
	}

	jobType := cron.JobTypeCron
	if req.Type == "once" {
		jobType = cron.JobTypeOnce
	}

	job := &cron.Job{
		Name:     req.Name,
		Type:     jobType,
		Schedule: req.Schedule,
		Prompt:   req.Prompt,
		UserID:   req.UserID,
	}

	if err := s.scheduler.AddJob(r.Context(), job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加任务失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          job.ID,
		"name":        job.Name,
		"next_run_at": job.NextRunAt,
	})
}

// handleDeleteCronJob 删除定时任务
func (s *Server) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.RemoveJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已删除"})
}

// handlePauseCronJob 暂停定时任务
func (s *Server) handlePauseCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.PauseJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "暂停任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已暂停"})
}

// handleResumeCronJob 恢复定时任务
func (s *Server) handleResumeCronJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := s.scheduler.ResumeJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "恢复任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已恢复"})
}
