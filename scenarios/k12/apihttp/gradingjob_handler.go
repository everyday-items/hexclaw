package apihttp

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 统一 GradingJob HTTP 契约（架构设计 §6.7 公共命令 / §4.10 幂等 / §5.4 字段）。
// 桌面、API、IM 私聊和工作流共用这组命令；两阶段 recognize/grade 自编排入口在
// 批改链路切换（执行计划 §4.1 链路①）时收敛删除，本轮不动。

// --- DTO ---

type gradingCheckpointDTO struct {
	Stage          string `json:"stage"`
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	RecordedAt     int64  `json:"recorded_at,omitempty"`
	Degraded       bool   `json:"degraded,omitempty"`
}

type gradingJobDTO struct {
	JobID             string                   `json:"job_id"` // = record_id（类型化 k12_grading_jobs 落地前的稳定标识）
	SubmissionID      string                   `json:"submission_id"`
	Stage             string                   `json:"stage"`
	ConfirmationState string                   `json:"confirmation_state"`
	AnchorState       string                   `json:"anchor_state"`
	Deadline          int64                    `json:"deadline"` // 0 = 人工等待不计入（§6.7 规则 7）
	ModelSnapshot     k12.GradingModelSnapshot `json:"model_snapshot"`
	IdempotencyKey    string                   `json:"idempotency_key"`
	ConfirmedVersion  int                      `json:"confirmed_version"`
	StageCheckpoints  []gradingCheckpointDTO   `json:"stage_checkpoints,omitempty"`
	AttemptCount      int                      `json:"attempt_count"`
	FailureKind       string                   `json:"failure_kind,omitempty"`
	Retryable         bool                     `json:"retryable"`
	Version           int                      `json:"version"`
	CreatedAt         int64                    `json:"created_at"`
	UpdatedAt         int64                    `json:"updated_at"`
	// RecognizedQuestions 只在 GET 单任务详情的识别停点附带；创建/列表不扩大载荷。
	RecognizedQuestions []recognizedQuestionDTO `json:"recognized_questions,omitempty"`
}

func toGradingJobDTO(v usecase.GradingJobView) gradingJobDTO {
	cps := make([]gradingCheckpointDTO, 0, len(v.Fields.StageCheckpoints))
	for _, cp := range v.Fields.StageCheckpoints {
		cps = append(cps, gradingCheckpointDTO{
			Stage: cp.Stage, ArtifactDigest: cp.ArtifactDigest, RecordedAt: cp.RecordedAt, Degraded: cp.Degraded,
		})
	}
	return gradingJobDTO{
		JobID: v.Record.RecordID, SubmissionID: v.Fields.SubmissionID, Stage: v.Record.Status,
		ConfirmationState: v.Fields.ConfirmationState, AnchorState: v.Fields.AnchorState,
		Deadline: v.Fields.Deadline, ModelSnapshot: v.Fields.ModelSnapshot,
		IdempotencyKey: v.Fields.IdempotencyKey, ConfirmedVersion: v.Fields.ConfirmedVersion,
		StageCheckpoints: cps, AttemptCount: v.Fields.AttemptCount,
		FailureKind: v.Fields.FailureKind, Retryable: v.Fields.Retryable,
		Version: v.Record.Version, CreatedAt: v.Record.CreatedAt, UpdatedAt: v.Record.UpdatedAt,
	}
}

// writeGradingJob 单 Job 响应：顶层平铺 stage/confirmation_state/anchor_state 便于命令响应断言，
// 全量字段在 job 内。
func writeGradingJob(w http.ResponseWriter, v usecase.GradingJobView) {
	dto := toGradingJobDTO(v)
	writeJSON(w, http.StatusOK, map[string]any{
		"stage": dto.Stage, "confirmation_state": dto.ConfirmationState, "anchor_state": dto.AnchorState,
		"job": dto,
	})
}

// --- handlers ---

type createGradingJobReq struct {
	Agent            string                   `json:"agent"`
	SourceSession    string                   `json:"source_session"`
	SubmissionID     string                   `json:"submission_id"`
	SourceKind       string                   `json:"source_kind"` // desktop/http/im/workflow（§4.10 统一幂等键来源）
	SourceKey        string                   `json:"source_key"`
	ConfirmedVersion int                      `json:"confirmed_version"`
	ModelSnapshot    k12.GradingModelSnapshot `json:"model_snapshot"`
	// ImageBase64 桌面拍照批改入口（§3.4 入口自编排收敛）：携带作业原图即创建照片批改 Job
	// 并异步启动编排器推进（推进到 awaiting_confirmation 停点，前端轮询 GET）。
	// 图片载体 = base64 / data URL（decodeRequiredImage）；原图不入 records Fields JSON，
	// 由编排器落本地文件 + source-scoped SubmissionID 中的图片摘要做内容校验；
	// 旧 photo-sha1 任务仍可恢复（§6.15 设计申报）。
	ImageBase64 string `json:"image_base64,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Grade       string `json:"grade,omitempty"`
}

// createGradingJob POST /grading-jobs —— 创建（幂等：同键返回既有 Job，created=false）。
// 带 image_base64 时走照片批改路径：创建 + 异步启动编排器（goroutine 与请求 context 解耦、
// 有界并发、panic 不逃逸，§6.15 任务执行模型）。
func (h *handler) createGradingJob(w http.ResponseWriter, r *http.Request) {
	var req createGradingJobReq
	if !decodeLimit(w, r, &req, 8<<20) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	if req.ImageBase64 != "" {
		h.createPhotoGradingJob(w, r, req)
		return
	}
	if h.rt.ModelSnapshotResolver == nil {
		writeErr(w, http.StatusInternalServerError, "grading model snapshot resolver 未配置")
		return
	}
	modelSnapshot, err := h.rt.ModelSnapshotResolver(req.ModelSnapshot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, created, err := h.rt.Deps.CreateGradingJob(r.Context(), req.Agent, req.SourceSession, usecase.CreateGradingJobInput{
		SubmissionID: req.SubmissionID, SourceKind: req.SourceKind, SourceKey: req.SourceKey,
		ConfirmedVersion: req.ConfirmedVersion, ModelSnapshot: modelSnapshot,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "job": toGradingJobDTO(v)})
}

// createPhotoGradingJob 照片批改 Job：固化原图 → 创建 Job（幂等）→ 异步推进到确认停点。
func (h *handler) createPhotoGradingJob(w http.ResponseWriter, r *http.Request, req createGradingJobReq) {
	if h.rt.Grading == nil {
		writeErr(w, http.StatusNotImplemented, "grading orchestrator 未注入（桌面照片批改不可用）")
		return
	}
	img, err := decodeRequiredImage(req.ImageBase64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceKind := req.SourceKind
	if sourceKind == "" {
		sourceKind = "desktop"
	}
	v, created, err := h.rt.Grading.StartPhotoGradingJob(r.Context(), usecase.StartPhotoGradingInput{
		Photo: usecase.PhotoGradeRequest{
			AgentName: req.Agent, Subject: req.Subject,
			Grade:         h.resolveGrade(r.Context(), req.Agent, req.Grade),
			SourceSession: req.SourceSession, Image: img,
		},
		SourceKind: sourceKind, SourceKey: req.SourceKey,
		ModelSnapshot: req.ModelSnapshot,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	h.rt.Grading.StartAsync(v.Record.RecordID)
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "job": toGradingJobDTO(v)})
}

// listGradingJobs GET /grading-jobs?agent=&stage= —— 查询任务与阶段。
func (h *handler) listGradingJobs(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ListGradingJobs(r.Context(), agent, r.URL.Query().Get("stage"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	out := make([]gradingJobDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toGradingJobDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getGradingJob GET /grading-jobs/{id}?agent= —— 单任务详情（跨实例 404）。
func (h *handler) getGradingJob(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	v, err := h.rt.Deps.GetGradingJob(r.Context(), agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	dto := toGradingJobDTO(v)
	resp := map[string]any{
		"job_id": dto.JobID, "stage": dto.Stage,
		"confirmation_state": dto.ConfirmationState, "anchor_state": dto.AnchorState,
		"deadline": dto.Deadline, "confirmed_version": dto.ConfirmedVersion,
		"job": dto,
	}
	// 编排器在场时附带识别停点产物（护栏回显数据源）。终态批改产物只由
	// GET /grading-jobs/{id}/result 返回，避免详情轮询隐式扩大结果契约。
	if h.rt.Grading != nil {
		if qs, ok := h.rt.Grading.RecognizedQuestionsForOwner(r.Context(), agent, jobID); ok {
			out := make([]recognizedQuestionDTO, 0, len(qs))
			for _, q := range qs {
				out = append(out, recognizedQuestionToDTO(q, true))
			}
			dto.RecognizedQuestions = out
			resp["job"] = dto
			resp["recognition"] = map[string]any{"questions": out, "subject": dominantSubject(qs)}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// getGradingJobResult GET /grading-jobs/{id}/result?agent= —— 独立读取终态产物。
// 先按 agent 校验归属；未完成或产物尚不可读均返回 409，调用方继续轮询 Job 状态。
func (h *handler) getGradingJobResult(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	v, err := h.rt.Deps.GetGradingJob(r.Context(), agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	if v.Record.Status != k12.GradingStageCompleted {
		writeErr(w, http.StatusConflict, "grading job 尚未完成")
		return
	}
	if h.rt.Grading == nil {
		writeErr(w, http.StatusConflict, "grading result unavailable")
		return
	}
	result, ok := h.rt.Grading.PhotoResult(jobID)
	if !ok {
		writeErr(w, http.StatusConflict, "grading result unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": jobID,
		"result": photoResultDTO(result),
	})
}

// photoItemDTO 逐题批改结果（判定五值口径；批改/解题分叉共用 gradeResp wire 形状）。
type photoItemDTO struct {
	Question    recognizedQuestionDTO        `json:"question"`
	Status      string                       `json:"status"`
	ResultKind  string                       `json:"result_kind"`
	Warning     string                       `json:"warning,omitempty"`
	Grade       *gradeResp                   `json:"grade,omitempty"`
	ParentGuide *usecase.ParentTeachingGuide `json:"parent_guide,omitempty"`
}

func photoResultDTO(res usecase.PhotoGradeResult) map[string]any {
	items := make([]photoItemDTO, 0, len(res.Items))
	for _, it := range res.Items {
		d := photoItemDTO{
			Question: recognizedQuestionToDTO(it.Recognized, true),
			Status:   string(it.Status), ResultKind: string(it.EffectiveResultKind()),
			Warning: it.Warning, ParentGuide: it.ParentGuide,
		}
		switch {
		case it.Status == usecase.PhotoBlankSolved,
			it.Status == usecase.PhotoOutOfScope && it.Grade.Solution == "" && it.Solve.OutOfScope:
			g := gradeResp{
				Solution: it.Solve.Solution, Verdict: string(it.Solve.Evidence.Verdict),
				EvidenceType: string(it.Solve.Evidence.EvidenceType), Badge: it.Solve.Evidence.Badge(),
				OutOfScope: it.Solve.OutOfScope, OutOfScopeKP: it.Solve.OutOfScopeKP, SolveOnly: true,
			}
			d.Grade = &g
		case it.Grade.Solution != "" || it.Grade.Outcome.Verdict != "" || it.Grade.OutOfScope || it.Grade.Evidence.Verdict != "":
			g := gradeRespFromResult(it.Grade)
			d.Grade = &g
		}
		items = append(items, d)
	}
	out := map[string]any{
		"mode": string(res.Mode), "items": items,
		"task_intent": string(res.EffectiveTaskIntent()), "result_surface": string(res.EffectiveResultSurface()),
		"markdown": res.Markdown, "image_warning": res.ImageWarning,
	}
	if res.AnnotatedImage != nil && len(res.AnnotatedImage.Data) > 0 {
		sum := sha256.Sum256(res.AnnotatedImage.Data)
		mime := strings.TrimSpace(res.AnnotatedImage.MIME)
		if mime == "" {
			mime = "application/octet-stream"
		}
		out["annotated_image"] = map[string]any{
			"mime": mime, "data_base64": base64.StdEncoding.EncodeToString(res.AnnotatedImage.Data),
			"digest": fmt.Sprintf("sha256:%x", sum[:]),
		}
	}
	return out
}

type confirmGradingJobReq struct {
	Agent       string   `json:"agent"`
	Corrections []string `json:"corrections"` // 批量确认/修正识别（空 = 全按识别结果确认）
	// 结构化确认载荷（桌面护栏）：整卷学科/年级 + 逐题修正，应用到已固化识别产物后
	// 冻结 canonical 输入（§6.7 规则 1/6），编排器异步续跑到终态。
	Subject             string                              `json:"subject,omitempty"`
	Grade               string                              `json:"grade,omitempty"`
	QuestionCorrections []usecase.GradingQuestionCorrection `json:"question_corrections,omitempty"`
}

// confirmGradingJob POST /grading-jobs/{id}/confirm —— 批量确认/修正识别结果；
// 确认冻结 canonical 输入，与锚点汇合后进 assessing（§6.7 规则 1）。
// 编排器在场且该 Job 有在途运行时：应用结构化修正并**异步**续跑；否则纯状态机确认。
func (h *handler) confirmGradingJob(w http.ResponseWriter, r *http.Request) {
	var req confirmGradingJobReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	// 多孩隔离硬边界：编排器按 jobID 寻址，进入前必须先按请求 agent 校验归属（跨实例 404）。
	job, err := h.rt.Deps.GetGradingJob(r.Context(), req.Agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	if h.rt.Grading != nil {
		input := usecase.ConfirmPhotoGradingInput{
			Subject: req.Subject, Grade: h.resolveGrade(r.Context(), req.Agent, req.Grade),
			Corrections: req.QuestionCorrections,
		}
		v, ok, err := h.rt.Grading.ConfirmPersistedTextGradingJob(r.Context(), req.Agent, jobID, input)
		if ok {
			if err != nil {
				writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
				return
			}
			writeGradingJob(w, v)
			return
		}
		v, ok, err = h.rt.Grading.ConfirmPhotoGradingJob(r.Context(), jobID, input)
		if ok {
			if err != nil {
				writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
				return
			}
			writeGradingJob(w, v)
			return
		}
	}
	if job.Fields.SourceKind == "webhook" && strings.HasPrefix(job.Fields.SubmissionID, "webhook-receipt:") {
		writeErr(w, http.StatusNotImplemented, "webhook 文本批改 worker 未注入")
		return
	}
	v, err := h.rt.Deps.ConfirmGradingJob(r.Context(), req.Agent, jobID, req.Corrections)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeGradingJob(w, v)
}

type reviseGradingJobReq struct {
	Agent         string   `json:"agent"`
	SourceSession string   `json:"source_session"`
	Corrections   []string `json:"corrections"`
}

// reviseGradingJob POST /grading-jobs/{id}/revise —— completed 后修正 → confirmed_version+1
// 新 Job 走 queued→assessing 捷径（§6.7 规则 6）；旧 Job 不回退。
func (h *handler) reviseGradingJob(w http.ResponseWriter, r *http.Request) {
	var req reviseGradingJobReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, created, err := h.rt.Deps.ReviseGradingJob(r.Context(), req.Agent, r.PathValue("id"), req.SourceSession, req.Corrections)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "job": toGradingJobDTO(v)})
}

// cancelGradingJob POST /grading-jobs/{id}/cancel —— 可取消态判定见 §6.7 规则 5/7；
// rendering/projecting 拒绝（409）。
func (h *handler) cancelGradingJob(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	if h.rt.Grading != nil {
		v, ok, err := h.rt.Grading.CancelPhotoGradingJob(r.Context(), req.Agent, jobID)
		if ok {
			if err != nil {
				writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
				return
			}
			writeGradingJob(w, v)
			return
		}
	}
	v, err := h.rt.Deps.CancelGradingJob(r.Context(), req.Agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeGradingJob(w, v)
}

// retryGradingJob POST /grading-jobs/{id}/retry —— 安全重试（仅 failed_retryable 且 retryable），
// 回 queued 从最近检查点恢复（§6.7 规则 3/4）。编排器在场且有在途运行时：异步续跑。
func (h *handler) retryGradingJob(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	// 多孩隔离硬边界：同 confirm——先按请求 agent 校验归属（跨实例 404）。
	if _, err := h.rt.Deps.GetGradingJob(r.Context(), req.Agent, jobID); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	if h.rt.Grading != nil {
		v, ok, err := h.rt.Grading.RetryPhotoGradingJob(r.Context(), jobID)
		if ok {
			if err != nil {
				writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
				return
			}
			writeGradingJob(w, v)
			return
		}
	}
	v, err := h.rt.Deps.RetryGradingJob(r.Context(), req.Agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeGradingJob(w, v)
}

type advanceGradingJobReq struct {
	Agent          string `json:"agent"`
	Outcome        string `json:"outcome"` // ok / failed / anchor
	ArtifactDigest string `json:"artifact_digest"`
	FailureKind    string `json:"failure_kind"`
	Retryable      bool   `json:"retryable"`
	AnchorState    string `json:"anchor_state"`
}

// advanceGradingJob POST /grading-jobs/{id}/advance —— 内部阶段推进（编排器专用）。
// 桌面/钉钉不得调用：它们只用上面的公共命令。本端点是执行侧回写一个阶段的完成/失败/锚点
// 结果的契约入口；进程内编排器（usecase.GradingOrchestrator）已接线并承接钉钉入口，
// 桌面入口迁移待前端协调窗口（前端 store/RecognizeGuardPanel 为并行会话活跃区）。
func (h *handler) advanceGradingJob(w http.ResponseWriter, r *http.Request) {
	var req advanceGradingJobReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.AdvanceGradingStage(r.Context(), req.Agent, r.PathValue("id"), usecase.AdvanceGradingInput{
		Outcome: req.Outcome, ArtifactDigest: req.ArtifactDigest,
		FailureKind: req.FailureKind, Retryable: req.Retryable, AnchorState: req.AnchorState,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeGradingJob(w, v)
}
