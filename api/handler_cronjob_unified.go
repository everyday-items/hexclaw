// handler_cronjob_unified 实现 v0.4.x cron 统一 endpoint (D1.2)。
//
// POST /api/v1/cronjob — 单一入口，7 action 全覆盖
//
//	action=create   创建任务（带 idempotency_key 防重复 + 30/user 配额）
//	action=update   更新任务（remove + create 原子替换）
//	action=list     列出任务
//	action=pause    暂停
//	action=resume   恢复
//	action=remove   删除
//	action=run      立即触发执行（fire-and-forget）
//
// 设计要点：
//   - idempotency_key：5min in-memory cache，同 (user_id, idempotency_key) 同响应
//   - 配额：create 前检查活跃任务数 < QuotaPerUser
//   - 错误：统一 APIError，永不暴露 stack/tool_use_id（D2.3 humanizeError 配套）
//   - 兼容：旧 /api/v1/cron/jobs* endpoints 仍保留，等前端切换后可删
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/cron"
)

// CronQuotaPerUser 每用户活跃 cron 任务上限（决策点 2：B 30/user，K12 教辅放宽）。
const CronQuotaPerUser = 30

// CronJobRequest 统一入口请求体。
type CronJobRequest struct {
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	UserID         string `json:"user_id,omitempty"`

	// create / update payload
	Draft *CronJobDraft `json:"draft,omitempty"`

	// pause / resume / remove / run / update target
	JobID string `json:"job_id,omitempty"`

	// list filter
	IncludePaused bool `json:"include_paused,omitempty"`
}

// CronJobResponse 统一响应体（按 action 不同填不同字段）。
type CronJobResponse struct {
	Action string      `json:"action"`
	Job    *cron.Job   `json:"job,omitempty"`
	Jobs   []*cron.Job `json:"jobs,omitempty"`
	Total  int         `json:"total,omitempty"`
	Quota  *quotaInfo  `json:"quota,omitempty"`
	OK     bool        `json:"ok,omitempty"`
}

type quotaInfo struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// idempCache 5min TTL idempotency cache（防 K12 家长 spam 重复提交）。
type idempCache struct {
	mu      sync.Mutex
	entries map[string]idempEntry
}

type idempEntry struct {
	expiresAt time.Time
	status    int
	body      []byte
}

var cronIdempCache = &idempCache{entries: make(map[string]idempEntry)}

func (c *idempCache) get(key string) (int, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return 0, nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return 0, nil, false
	}
	return e.status, e.body, true
}

func (c *idempCache) put(key string, status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = idempEntry{
		expiresAt: time.Now().Add(5 * time.Minute),
		status:    status,
		body:      body,
	}
	// 顺便清理过期条目（amortized O(N)，对小规模缓存 OK）
	if len(c.entries) > 1000 {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
}

// idempKey 组合 user + key（不同用户的 key 互不影响）。
func idempKey(userID, key string) string {
	return userID + "::" + key
}

// handleCronjobUnified POST /api/v1/cronjob 统一入口（D1.2）。
func (s *Server) handleCronjobUnified(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, CodeCronDisabled, "定时任务未启用")
		return
	}

	var req CronJobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误: "+humanizeError(err))
		return
	}
	if req.UserID == "" {
		req.UserID = "api-user"
	}

	// idempotency check（只对写操作生效）
	if req.IdempotencyKey != "" && isMutationAction(req.Action) {
		if status, body, hit := cronIdempCache.get(idempKey(req.UserID, req.IdempotencyKey)); hit {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Idempotent-Replay", "1")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
	}

	ctx := r.Context()
	resp, status, err := s.dispatchCronjobAction(ctx, &req)
	if err != nil {
		writeAPIError(w, status, errToCode(err), humanizeError(err))
		return
	}

	body, _ := json.Marshal(resp)
	if req.IdempotencyKey != "" && isMutationAction(req.Action) {
		cronIdempCache.put(idempKey(req.UserID, req.IdempotencyKey), status, body)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func isMutationAction(action string) bool {
	switch action {
	case "create", "update", "remove", "pause", "resume", "run":
		return true
	}
	return false
}

// dispatchCronjobAction 按 action 分发到具体实现。
func (s *Server) dispatchCronjobAction(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	switch req.Action {
	case "create":
		return s.cronActionCreate(ctx, req)
	case "update":
		return s.cronActionUpdate(ctx, req)
	case "list":
		return s.cronActionList(ctx, req)
	case "pause":
		return s.cronActionPause(ctx, req)
	case "resume":
		return s.cronActionResume(ctx, req)
	case "remove":
		return s.cronActionRemove(ctx, req)
	case "run":
		return s.cronActionRun(ctx, req)
	default:
		return nil, http.StatusBadRequest, &cronErr{code: CodeCronUnknownAction, msg: "未知 action: " + req.Action}
	}
}

func (s *Server) cronActionCreate(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.Draft == nil {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "draft 必填"}
	}
	d := req.Draft
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Schedule) == "" || strings.TrimSpace(d.Prompt) == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "name、schedule、prompt 必填"}
	}

	// D1.3 配额检查：用户活跃任务数 < CronQuotaPerUser
	used, err := s.activeCronJobCount(ctx, req.UserID)
	if err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: "配额检查失败"}
	}
	if used >= CronQuotaPerUser {
		return nil, http.StatusTooManyRequests, &cronErr{
			code: CodeCronQuotaExceeded,
			msg:  "活跃定时任务已达上限（" + cronItoa(CronQuotaPerUser) + " 个）—— 请删除旧任务后重试",
		}
	}

	// First-class pre-authored script path (BUG-20260615): when the draft carries
	// a script, admit it directly as a deterministic script-mode job — skip LLM
	// compilation, which a weak model keeps failing for mechanical collectors.
	var job *cron.Job
	if strings.TrimSpace(d.Script) != "" {
		job, err = s.scheduler.AddJobFromScript(ctx, cron.AddJobRequest{
			Name:       d.Name,
			Schedule:   d.Schedule,
			Prompt:     d.Prompt,
			UserID:     req.UserID,
			Deliver:    d.Deliver,
			ChatID:     d.ChatID, // IM/连接投递目标会话/群组 ID（Deliverer 必需）
			TimeoutSec: d.TimeoutSec,
			Paused:     d.Paused, // 审批未决 → 初始暂停，授权后 resume
		}, d.Runtime, d.Script)
	} else {
		job, err = s.scheduler.AddJobFromPrompt(ctx, cron.AddJobRequest{
			Name:         d.Name,
			Schedule:     d.Schedule,
			Prompt:       d.Prompt,
			UserID:       req.UserID,
			Deliver:      d.Deliver, // D4.2 多 deliver 桥接
			ChatID:       d.ChatID,  // IM/连接投递目标会话/群组 ID（Deliverer 必需）
			TimeoutSec:   d.TimeoutSec,
			Continuous:   d.Continuous, // 持续型任务（强制 agent 模式）
			Paused:       d.Paused,     // 审批未决 → 初始暂停，授权后 resume
			LocalAPIBase: s.localAPIBase(),
		})
	}
	if err != nil {
		status, code := classifyCronCreateError(err)
		// Full-fidelity cause to the log; humanized message to the client
		// (BUG-20260611 finding #3: never blame user parameters for model flakes).
		slog.Warn("[cron-create] unified failed", "source", "cron", "user", req.UserID, "name", d.Name,
			"stage", stageOfCreateError(err), "code", code, "err", err.Error())
		return nil, status, &cronErr{code: code, msg: humanizeError(err)}
	}
	slog.Info("[cron-create] unified done", "source", "cron", "user", req.UserID, "job_id", job.ID, "runtime", job.Spec.Runtime)

	return &CronJobResponse{
		Action: "create",
		Job:    job,
		Quota:  &quotaInfo{Used: used + 1, Limit: CronQuotaPerUser},
		OK:     true,
	}, http.StatusOK, nil
}

func (s *Server) cronActionUpdate(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.JobID == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "job_id 必填"}
	}
	// 更新 = 原子替换：先删后建（共享 idempotency_key 保证幂等）
	//
	// Capture the original before removal: if the create side fails (compile
	// error, agent frequency guard, quota, ...), the original job must be
	// restored — otherwise update silently deletes the user's job (review M4).
	var origCopy *cron.Job
	if orig, ok := s.scheduler.GetJob(ctx, req.JobID); ok {
		c := *orig
		origCopy = &c
	}
	if err := s.scheduler.RemoveJob(ctx, req.JobID); err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: "更新前清理失败"}
	}
	resp, status, err := s.cronActionCreate(ctx, req)
	if err != nil && origCopy != nil {
		if rerr := s.scheduler.AddJob(ctx, origCopy); rerr != nil {
			slog.Error("[cron-update] failed to restore original job after create failure",
				"source", "cron", "job_id", req.JobID, "err", rerr.Error())
		}
	}
	return resp, status, err
}

func (s *Server) cronActionList(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	jobs, err := s.scheduler.ListJobs(ctx, req.UserID)
	if err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: humanizeError(err)}
	}
	used := 0
	filtered := jobs[:0]
	for _, j := range jobs {
		if !req.IncludePaused && j.Status == cron.StatusPaused {
			continue
		}
		if j.Status == cron.StatusActive {
			used++
		}
		filtered = append(filtered, j)
	}
	return &CronJobResponse{
		Action: "list",
		Jobs:   filtered,
		Total:  len(filtered),
		Quota:  &quotaInfo{Used: used, Limit: CronQuotaPerUser},
	}, http.StatusOK, nil
}

func (s *Server) cronActionPause(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.JobID == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "job_id 必填"}
	}
	if err := s.scheduler.PauseJob(ctx, req.JobID); err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: humanizeError(err)}
	}
	return &CronJobResponse{Action: "pause", OK: true}, http.StatusOK, nil
}

func (s *Server) cronActionResume(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.JobID == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "job_id 必填"}
	}
	if err := s.scheduler.ResumeJob(ctx, req.JobID); err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: humanizeError(err)}
	}
	return &CronJobResponse{Action: "resume", OK: true}, http.StatusOK, nil
}

func (s *Server) cronActionRemove(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.JobID == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "job_id 必填"}
	}
	if err := s.scheduler.RemoveJob(ctx, req.JobID); err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: humanizeError(err)}
	}
	// 授权生命周期跟随任务：删除即回收其全部任务级授权。
	s.revokeTaskGrants(ctx, "cron:"+req.JobID)
	return &CronJobResponse{Action: "remove", OK: true}, http.StatusOK, nil
}

func (s *Server) cronActionRun(ctx context.Context, req *CronJobRequest) (*CronJobResponse, int, error) {
	if req.JobID == "" {
		return nil, http.StatusBadRequest, &cronErr{code: CodeBadRequest, msg: "job_id 必填"}
	}
	if err := s.scheduler.TriggerJob(ctx, req.JobID); err != nil {
		return nil, http.StatusInternalServerError, &cronErr{code: CodeInternalError, msg: humanizeError(err)}
	}
	return &CronJobResponse{Action: "run", OK: true}, http.StatusOK, nil
}

// activeCronJobCount 当前用户活跃（非 paused）任务数。
func (s *Server) activeCronJobCount(ctx context.Context, userID string) (int, error) {
	jobs, err := s.scheduler.ListJobs(ctx, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Status == cron.StatusActive {
			n++
		}
	}
	return n, nil
}

// cronErr 内部错误类型，承载 code + 友好 msg。
type cronErr struct {
	code string
	msg  string
}

func (e *cronErr) Error() string { return e.msg }

func errToCode(err error) string {
	var ce *cronErr
	if errors.As(err, &ce) {
		return ce.code
	}
	return CodeInternalError
}

// cronItoa 极简整数转字符串，不引 strconv（保持小依赖）。
func cronItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
