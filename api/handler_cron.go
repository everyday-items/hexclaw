package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon/runtime"
	"github.com/hexagon-codes/hexclaw/cron"
)

// --- Cron API ---

// AddCronJobRequest 添加定时任务请求
//
// v2：Prompt 仍是用户自然语言需求；服务端会调 LLM 编译成 JobSpec（Python 脚本）。
// Type 仅支持 "cron"（一次性任务暂未接入编译路径）。
type AddCronJobRequest struct {
	Name     string `json:"name"`     // 任务名称
	Schedule string `json:"schedule"` // cron 表达式或 @every/@daily 等
	Prompt   string `json:"prompt"`   // 自然语言任务需求（会被编译成脚本）
	UserID   string `json:"user_id"`
	Type     string `json:"type"` // cron 或 once（once 暂未接入编译）
	Platform string `json:"platform,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	// Deliver 显式投递目标（chat/push/feishu/discord/wechat 或已配置连接实例 id/name 的任意组合）。
	// 留空 → 后端按 prompt 由 LLM 推导（默认 ["chat"]）；非空 → 直接覆盖，前端「连接库下拉」即走此字段（§5 一处存处处引）。
	Deliver []string `json:"deliver,omitempty"`
	// Continuous 开启「持续型任务 + 跨 tick 检查点」：长目标分多次廉价唤醒累积推进（强制 agent 模式）。
	Continuous bool `json:"continuous,omitempty"`
}

// handleListCronJobs 列出定时任务
func (s *Server) handleListCronJobs(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []any{}, "total": 0})
		return
	}
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

// handleAddCronJob 添加定时任务。
//
// 按 Accept header 双模式：
//   - text/event-stream → SSE 流式编译，实时推送 progress/done/error 事件
//   - 默认（application/json）→ 同步阻塞，等编译完成后一次性返回 JSON
//
// 双模式共用同一 AddCronJobRequest 与同一业务逻辑（Scheduler.AddJobFromPromptWithProgress）。
func (s *Server) handleAddCronJob(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleAddCronJobSSE(w, r)
		return
	}
	s.handleAddCronJobJSON(w, r)
}

// handleAddCronJobJSON 经典阻塞模式（保留给非浏览器客户端 / curl / 老前端）。
func (s *Server) handleAddCronJobJSON(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseAddCronJobRequest(w, r)
	if !ok {
		return
	}

	job, err := s.scheduler.AddJobFromPrompt(r.Context(), cron.AddJobRequest{
		Name:         req.Name,
		Schedule:     req.Schedule,
		Prompt:       req.Prompt,
		UserID:       req.UserID,
		Platform:     req.Platform,
		ChatID:       req.ChatID,
		Deliver:      req.Deliver,
		Continuous:   req.Continuous,
		LocalAPIBase: s.localAPIBase(),
	})
	if err != nil {
		status, code := classifyCronCreateError(err)
		// D2.3 错误友好化 — 永不把 openai api error / 500 / request_id / tool_use_id 暴露给用户
		writeAPIError(w, status, code, "添加任务失败: "+humanizeError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":          job,
		"spec_preview": job.Spec,
	})
}

// handleAddCronJobSSE 流式模式：每完成一个编译阶段就 flush 一帧。
//
// 事件格式（标准 W3C SSE）：
//
//	event: progress
//	data: {"stage":"calling_llm","message":"调用 LLM…"}
//
//	event: done
//	data: {"job":{...},"spec_preview":{...}}
//
//	event: error
//	data: {"error":"..."}
//
// 客户端断流（fetch abort / EventSource.close）= 取消，r.Context() 会随之 Done。
func (s *Server) handleAddCronJobSSE(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseAddCronJobRequest(w, r)
	if !ok {
		return
	}

	// C-5 修复：SSE 创建路径与 unified create（cronActionCreate）共用同一 30/user 配额闸。
	// 否则桌面端主路径（恒走 SSE）可无限建任务、绕过上限。在 SSE 头写出前做检查，
	// 越限直接返 429（前端 apiSSE 对非 2xx 抛错），与 unified 行为一致。
	used, err := s.activeCronJobCount(r.Context(), req.UserID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, CodeInternalError, "配额检查失败")
		return
	}
	if used >= CronQuotaPerUser {
		writeAPIError(w, http.StatusTooManyRequests, CodeCronQuotaExceeded,
			"活跃定时任务已达上限（"+cronItoa(CronQuotaPerUser)+" 个）—— 请删除旧任务后重试")
		return
	}

	// hexagon SSEEventSink：headers + flush + 线程安全锁 + Close 语义都由它管。
	// 业务事件（progress/done/error）经 EmitRaw 走，wire 与改造前完全一致以保持
	// 前端契约不变（参考 hexclaw-desktop src/api/tasks.ts parseSSEFrame）。
	sink, err := runtime.NewSSEEventSink(w)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer sink.Close()
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	onProgress := func(p cron.CompileProgress) {
		_ = sink.EmitRaw(ctx, "progress", map[string]any{
			"stage":   string(p.Stage),
			"message": p.Message,
		})
	}

	slog.Info("[cron-create] sse start", "source", "cron", "user", req.UserID, "name", req.Name, "schedule", req.Schedule, "prompt_len", len(req.Prompt))
	startedAt := time.Now()
	job, err := s.scheduler.AddJobFromPromptWithProgress(ctx, cron.AddJobRequest{
		Name:         req.Name,
		Schedule:     req.Schedule,
		Prompt:       req.Prompt,
		UserID:       req.UserID,
		Platform:     req.Platform,
		ChatID:       req.ChatID,
		Deliver:      req.Deliver,
		Continuous:   req.Continuous,
		LocalAPIBase: s.localAPIBase(),
	}, onProgress)
	if err != nil {
		// Full-fidelity cause goes to the log; the SSE event carries the
		// humanized message plus the failed pipeline stage so the client can
		// render "failed at <stage>" (BUG-20260611 finding #3).
		slog.Warn("[cron-create] sse failed", "source", "cron", "user", req.UserID, "name", req.Name,
			"stage", stageOfCreateError(err), "duration_ms", time.Since(startedAt).Milliseconds(), "err", err.Error())
		_ = sink.EmitRaw(ctx, "error", map[string]string{
			"error": "添加任务失败: " + humanizeError(err),
			"stage": stageOfCreateError(err),
		})
		return
	}
	slog.Info("[cron-create] sse done", "source", "cron", "user", req.UserID, "job_id", job.ID,
		"runtime", job.Spec.Runtime, "duration_ms", time.Since(startedAt).Milliseconds())
	_ = sink.EmitRaw(ctx, "done", map[string]any{
		"job":          job,
		"spec_preview": job.Spec,
	})
}

// parseAddCronJobRequest 共用的 body 解析与基础校验。错误直接写回 w 返 false。
//
// 返 APIError 标准化结构（含 code + message + legacy error 兼容字段）。
func (s *Server) parseAddCronJobRequest(w http.ResponseWriter, r *http.Request) (*AddCronJobRequest, bool) {
	var req AddCronJobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误: "+err.Error())
		return nil, false
	}
	if req.Name == "" || req.Schedule == "" || req.Prompt == "" {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "name、schedule 和 prompt 不能为空")
		return nil, false
	}
	if req.UserID == "" {
		req.UserID = "api-user"
	}
	if s.scheduler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, CodeCronDisabled, "定时任务未启用")
		return nil, false
	}
	if req.Type == "once" {
		writeAPIError(w, http.StatusBadRequest, CodeCronNotSupported, "v2 暂不支持 once 类型；请使用 schedule 表达式")
		return nil, false
	}
	return &req, true
}

// stageOfCreateError infers which compile-pipeline stage a create error came
// from, so clients can render "failed at <stage>" (matching the SSE progress
// stages) instead of guessing from flattened text. Matching is by the stable
// Chinese error prefixes each pipeline node wraps with.
func stageOfCreateError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, cron.AgentIntervalGuardPrefix):
		// Agent-mode frequency guard fires before compilation; surface it as a
		// validation-stage rejection (review M2).
		return "validating"
	case strings.Contains(msg, "脚本校验失败"), strings.Contains(msg, "脚本含禁用调用"):
		return "validating"
	case strings.Contains(msg, "解析编译输出失败"), strings.Contains(msg, "LLM 编译失败"), strings.Contains(msg, "编译失败"):
		return "calling_llm"
	case strings.Contains(msg, "无效的调度表达式"), strings.Contains(msg, "保存任务失败"):
		return "persisting"
	default:
		return ""
	}
}

// classifyCronCreateError 把 AddJobFromPrompt 的错误映射到 (HTTP 状态码, APIError code)。
func classifyCronCreateError(err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, cron.AgentIntervalGuardPrefix):
		// User-correctable schedule problem, not a server fault (review M2).
		return http.StatusBadRequest, CodeCronInvalidSched
	case strings.Contains(msg, "编译失败"):
		return http.StatusBadRequest, CodeCronCompileFailed
	case strings.Contains(msg, "校验失败"):
		return http.StatusBadRequest, CodeCronValidateFailed
	case strings.Contains(msg, "无效的调度表达式") || strings.Contains(msg, "invalid"):
		return http.StatusBadRequest, CodeCronInvalidSched
	default:
		return http.StatusInternalServerError, CodeInternalError
	}
}

// localAPIBase 返回 hexclaw 自身 HTTP API 的 base URL，传给 compiler 让 LLM
// 在生成的脚本里能引用 /api/v1/knowledge/* 等本地端点。
//
// Reads the configured server port so compiled scripts target the actual
// listener instead of a hardcoded default (review L10).
func (s *Server) localAPIBase() string {
	port := 16060
	if s.cfg != nil && s.cfg.Server.Port > 0 {
		port = s.cfg.Server.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// handleDeleteCronJob 删除定时任务
func (s *Server) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "定时任务未启用"})
		return
	}
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
	if s.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "定时任务未启用"})
		return
	}
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
	if s.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "定时任务未启用"})
		return
	}
	jobID := r.PathValue("id")
	if err := s.scheduler.ResumeJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "恢复任务失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已恢复"})
}
