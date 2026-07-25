package apihttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 练习集 DTO（前端契约，PRD §3.8 / §5.5）。
type practiceItemDTO struct {
	ItemID              string   `json:"item_id"`
	SourceProblemID     string   `json:"source_problem_id,omitempty"`
	Subject             string   `json:"subject,omitempty"`
	AddedVia            string   `json:"added_via,omitempty"`
	Question            string   `json:"question_markdown"`
	ExpectedAnswer      string   `json:"expected_answer_markdown,omitempty"`
	VerificationStatus  string   `json:"verification_status"`
	VerificationBadge   string   `json:"verification_evidence,omitempty"`
	BlockedReason       string   `json:"blocked_reason,omitempty"`
	PaperSeq            int      `json:"paper_seq,omitempty"`
	Returned            bool     `json:"returned,omitempty"`
	ReturnIDs           []string `json:"return_ids,omitempty"`
	GenerationJobID     string   `json:"generation_job_id,omitempty"`
	VariantIndex        int      `json:"variant_index,omitempty"`
	RequestedDifficulty string   `json:"requested_difficulty,omitempty"`
	ActualDifficulty    string   `json:"actual_difficulty,omitempty"`
	// ResultCorrect 复批逐题结论（§3.8 第 3-4 条）：null=尚无结论。
	ResultCorrect *bool `json:"result_correct,omitempty"`
}

type practiceReturnAssetDTO struct {
	ReturnID   string   `json:"return_id"`
	AssetID    string   `json:"asset_id"`
	ItemIDs    []string `json:"item_ids"`
	ReturnedAt int64    `json:"returned_at"`
}

type practiceSetDTO struct {
	RecordID            string                   `json:"record_id"`
	Title               string                   `json:"title"`
	SourceKind          string                   `json:"source_kind"`
	Status              string                   `json:"status"`
	StatusLabel         string                   `json:"status_label"`
	Publishable         bool                     `json:"publishable"`
	QuestionSheet       string                   `json:"question_artifact_id,omitempty"`
	AnswerSheet         string                   `json:"answer_artifact_id,omitempty"`
	DeliveryStatus      string                   `json:"delivery_status"`
	DeliveryBatchID     string                   `json:"delivery_batch_id,omitempty"`
	SkippedBlockedCount int                      `json:"skipped_blocked_count,omitempty"`
	PaperNo             string                   `json:"paper_no,omitempty"`
	FinalizedAt         int64                    `json:"finalized_at,omitempty"`
	FinalizedVia        string                   `json:"finalized_via,omitempty"`
	Items               []practiceItemDTO        `json:"items"`
	ReturnAssets        []practiceReturnAssetDTO `json:"return_assets"`
}

type practicePrintJobDTO struct {
	PrintJobID         string `json:"print_job_id"`
	PracticeSetID      string `json:"practice_set_id,omitempty"`
	IdempotencyKey     string `json:"idempotency_key"`
	Status             string `json:"status"`
	PaperNo            string `json:"paper_no,omitempty"`
	ArtifactKind       string `json:"artifact_kind"`
	ArtifactID         string `json:"artifact_id"`
	QuestionArtifactID string `json:"question_artifact_id,omitempty"`
	AnswerArtifactID   string `json:"answer_artifact_id,omitempty"`
	SourceKind         string `json:"source_kind,omitempty"`
	SourceRef          string `json:"source_ref,omitempty"`
	Title              string `json:"title,omitempty"`
	SourceDigest       string `json:"source_digest"`
	AttemptCount       int    `json:"attempt_count"`
	NativeJobID        string `json:"native_job_id,omitempty"`
	NativeReceiptID    string `json:"native_receipt_id,omitempty"`
	PrinterSnapshot    any    `json:"printer_snapshot,omitempty"`
	FailureKind        string `json:"failure_kind,omitempty"`
	FailureDetail      string `json:"failure_detail,omitempty"`
	PreparedAt         int64  `json:"prepared_at"`
	PrintedAt          int64  `json:"printed_at,omitempty"`
	UpdatedAt          int64  `json:"updated_at"`
	Version            int    `json:"version"`
}

func toGenericPrintJobDTO(v usecase.GenericPrintView) practicePrintJobDTO {
	job, artifact := v.Job, v.Artifact
	var printer any
	if job.PrinterSnapshot != "" && job.PrinterSnapshot != "{}" {
		_ = json.Unmarshal([]byte(job.PrinterSnapshot), &printer)
	}
	return practicePrintJobDTO{
		PrintJobID: job.PrintJobID, IdempotencyKey: job.IdempotencyKey,
		Status: job.Status, ArtifactKind: artifact.SourceKind, ArtifactID: artifact.ArtifactID,
		SourceKind: artifact.SourceKind, SourceRef: artifact.SourceRef, Title: artifact.Title,
		SourceDigest: artifact.SourceDigest, AttemptCount: job.AttemptCount,
		NativeJobID: job.NativeJobID, NativeReceiptID: job.NativeReceiptID,
		PrinterSnapshot: printer, FailureKind: job.FailureKind, FailureDetail: job.FailureDetail,
		PreparedAt: job.PreparedAt, PrintedAt: job.PrintedAt, UpdatedAt: job.UpdatedAt, Version: job.Version,
	}
}

func toPracticePrintJobDTO(v usecase.PracticePrintView) practicePrintJobDTO {
	job := v.Job
	var printer any
	if job.PrinterSnapshot != "" && job.PrinterSnapshot != "{}" {
		_ = json.Unmarshal([]byte(job.PrinterSnapshot), &printer)
	}
	return practicePrintJobDTO{
		PrintJobID: job.PrintJobID, PracticeSetID: job.PracticeSetID,
		IdempotencyKey: job.IdempotencyKey, Status: job.Status, PaperNo: job.PaperNo,
		ArtifactKind: job.ArtifactKind, ArtifactID: job.ArtifactID,
		QuestionArtifactID: job.QuestionArtifactID, AnswerArtifactID: job.AnswerArtifactID,
		SourceDigest: job.SourceDigest, AttemptCount: job.AttemptCount,
		NativeJobID: job.NativeJobID, NativeReceiptID: job.NativeReceiptID,
		PrinterSnapshot: printer, FailureKind: job.FailureKind, FailureDetail: job.FailureDetail,
		PreparedAt: job.PreparedAt, PrintedAt: job.PrintedAt, UpdatedAt: job.UpdatedAt, Version: job.Version,
	}
}

func toPracticeSetDTO(v usecase.PracticeSetView) practiceSetDTO {
	returnIDsByItem := make(map[string][]string)
	returnAssets := make([]practiceReturnAssetDTO, 0, len(v.Fields.ReturnAssets))
	for _, ra := range v.Fields.ReturnAssets {
		returnAssets = append(returnAssets, practiceReturnAssetDTO{
			ReturnID: ra.ReturnID, AssetID: ra.AssetID, ItemIDs: ra.ItemIDs, ReturnedAt: ra.ReturnedAt,
		})
		for _, itemID := range ra.ItemIDs {
			returnIDsByItem[itemID] = append(returnIDsByItem[itemID], ra.ReturnID)
		}
	}
	items := make([]practiceItemDTO, 0, len(v.Fields.Items))
	for _, it := range v.Fields.Items {
		items = append(items, practiceItemDTO{
			ItemID: it.ItemID, SourceProblemID: it.SourceProblemID, Subject: it.Subject,
			AddedVia: it.AddedVia,
			Question: it.QuestionMarkdown, ExpectedAnswer: it.ExpectedAnswerMarkdown,
			VerificationStatus: it.VerificationStatus, VerificationBadge: it.VerificationEvidence,
			BlockedReason: it.BlockedReason,
			PaperSeq:      it.PaperSeq, Returned: it.Returned, ReturnIDs: returnIDsByItem[it.ItemID],
			GenerationJobID: it.GenerationJobID, VariantIndex: it.VariantIndex,
			RequestedDifficulty: it.RequestedDifficulty, ActualDifficulty: it.ActualDifficulty,
			ResultCorrect: it.ResultCorrect,
		})
	}
	return practiceSetDTO{
		RecordID: v.Record.RecordID, Title: v.Fields.Title, SourceKind: v.Fields.SourceKind,
		Status: v.Record.Status, StatusLabel: k12.PracticeSetLabel(v.Record.Status),
		Publishable:         k12.PracticeSetPublishable(v.Fields),
		SkippedBlockedCount: v.Fields.SkippedBlockedCount,
		PaperNo:             v.Fields.PaperNo, FinalizedAt: v.Fields.FinalizedAt, FinalizedVia: v.Fields.FinalizedVia,
		QuestionSheet: v.Fields.QuestionArtifact, AnswerSheet: v.Fields.AnswerArtifact,
		DeliveryStatus: v.Fields.DeliveryStatus, DeliveryBatchID: v.Fields.DeliveryBatchID,
		Items: items, ReturnAssets: returnAssets,
	}
}

// POST /practice-sets（整卷直建）已随切换日死刑名单删除（执行计划 §3.4 端点冻结，
// 2026-07-18）：装篮命令（basket/items → finalize）是唯一 HTTP 创建路径；
// usecase.CreatePracticeSet 保留为装篮/自动装篮的内部原语。

func (h *handler) listPracticeSets(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ListPracticeSets(r.Context(), agent, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]practiceSetDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toPracticeSetDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getPracticeSet(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.GetPracticeSet(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toPracticeSetDTO(v))
}

// getPracticePaper GET /practice-sets/{id}/paper?kind=question|answer（§4.13 呈现物真实渲染）：
// 固化后返回正卷（页眉/页脚含卷面号）；draft 走同一渲染器返回预览（preview=true、无卷面号），
// 预览口径 = 固化产物口径。kind 缺省 question。
func (h *handler) getPracticePaper(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.RenderPracticePaper(r.Context(), agent, r.PathValue("id"), r.URL.Query().Get("kind"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": v.Kind, "title": v.Title, "paper_no": v.PaperNo,
		"markdown": v.Markdown, "preview": v.Preview,
	})
}

type verifyItemReq struct {
	Agent    string `json:"agent"`
	ItemID   string `json:"item_id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

func (h *handler) verifyPracticeItem(w http.ResponseWriter, r *http.Request) {
	var req verifyItemReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.ItemID == "" {
		writeErr(w, http.StatusBadRequest, "agent / item_id 必填")
		return
	}
	if err := h.rt.Deps.VerifyPracticeItem(r.Context(), req.Agent, r.PathValue("id"), req.ItemID, req.Status, req.Evidence); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadRequest), err.Error())
		return
	}
	h.respondPracticeSet(w, r, req.Agent)
}

type agentOnlyReq struct {
	Agent string `json:"agent"`
}

// addToBasket 装篮命令（2026-07-18 购物车裁决）：单 Learner 单篮、幂等去重。
type addToBasketReq struct {
	Agent         string          `json:"agent"`
	SourceSession string          `json:"source_session"`
	Item          practiceItemDTO `json:"item"`
}

func (h *handler) addToBasket(w http.ResponseWriter, r *http.Request) {
	var req addToBasketReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Item.Question == "" {
		writeErr(w, http.StatusBadRequest, "agent / item.question_markdown 必填")
		return
	}
	id, added, err := h.rt.Deps.AddToBasket(r.Context(), req.Agent, req.SourceSession, k12.PracticeItem{
		ItemID: req.Item.ItemID, SourceProblemID: req.Item.SourceProblemID, Subject: req.Item.Subject,
		AddedVia:         req.Item.AddedVia,
		QuestionMarkdown: req.Item.Question, ExpectedAnswerMarkdown: req.Item.ExpectedAnswer,
		VerificationStatus: req.Item.VerificationStatus, VerificationEvidence: req.Item.VerificationBadge,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadRequest), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record_id": id, "added": added})
}

type customPaperReq struct {
	Agent          string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
	Scope          string `json:"scope"`
	Total          any    `json:"total"`
	PerSource      int    `json:"per_source"`
	Difficulty     string `json:"difficulty"`
	Textbook       string `json:"textbook"`
	Grade          string `json:"grade,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
}

// generateCustomPaper 是 DD-027 唯一正式组卷命令。前端只提交一次冻结参数；生成、
// 验证、去重和装入待打印集合均由后端完成并原子提交。
func (h *handler) generateCustomPaper(w http.ResponseWriter, r *http.Request) {
	var req customPaperReq
	if !decode(w, r, &req) {
		return
	}
	total, ok := customPaperTotal(req.Total)
	if req.Agent == "" || req.IdempotencyKey == "" || !ok {
		writeErr(w, http.StatusBadRequest, "agent / idempotency_key / total(all/5/10) 必填")
		return
	}
	result, err := h.rt.Deps.GenerateCustomPaper(r.Context(), req.Agent, usecase.CustomPaperRequest{
		IdempotencyKey: req.IdempotencyKey, Scope: req.Scope, Total: total,
		PerSource: req.PerSource, Difficulty: req.Difficulty, Textbook: req.Textbook,
		Grade: req.Grade, SourceSession: req.SourceSession,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadGateway), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generation_job_id": result.GenerationJobID,
		"status":            result.Status,
		"set":               toPracticeSetDTO(result.Set),
		"items":             result.Items,
		"added":             result.Added,
		"deduplicated":      result.Deduplicated,
	})
}

func customPaperTotal(v any) (string, bool) {
	switch value := v.(type) {
	case string:
		if value == "all" || value == "5" || value == "10" {
			return value, true
		}
	case float64:
		if value == 5 || value == 10 {
			return strconv.Itoa(int(value)), true
		}
	}
	return "", false
}

type removeFromBasketReq struct {
	Agent  string `json:"agent"`
	ItemID string `json:"item_id"`
}

func (h *handler) removeFromBasket(w http.ResponseWriter, r *http.Request) {
	var req removeFromBasketReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.ItemID == "" {
		writeErr(w, http.StatusBadRequest, "agent / item_id 必填")
		return
	}
	if err := h.rt.Deps.RemoveFromBasket(r.Context(), req.Agent, r.PathValue("id"), req.ItemID); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	h.respondPracticeSet(w, r, req.Agent)
}

// finalizePracticeSet 固化出卷命令：打印/发送即确认，跳过阻断题（§3.8）。
type finalizeReq struct {
	Agent string `json:"agent"`
	Via   string `json:"via"` // print | send
}

func (h *handler) finalizePracticeSet(w http.ResponseWriter, r *http.Request) {
	var req finalizeReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, skipped, err := h.rt.Deps.FinalizeBasket(r.Context(), req.Agent, r.PathValue("id"), req.Via)
	if err != nil {
		if req.Via == "send" &&
			(errors.Is(err, usecase.ErrDeliveryUnavailable) ||
				errors.Is(err, usecase.ErrNoActiveDirectBindings)) {
			writeDeliveryError(w, err)
			return
		}
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	resp := map[string]any{"set": toPracticeSetDTO(v), "skipped_blocked_count": skipped}
	if v.Fields.DeliveryBatchID != "" {
		batch, batchErr := h.rt.Deps.GetDeliveryBatch(r.Context(), req.Agent, v.Fields.DeliveryBatchID)
		if batchErr != nil {
			writeDeliveryError(w, batchErr)
			return
		}
		resp["delivery_batch"] = batch
	}
	writeJSON(w, http.StatusOK, resp)
}

type preparePracticePrintReq struct {
	Agent          string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
	ArtifactKind   string `json:"artifact_kind"`
}

// preparePracticePrintJob is phase one: reserve a formal paper_no and immutable
// source, but leave the PracticeSet draft until a native printed receipt arrives.
func (h *handler) preparePracticePrintJob(w http.ResponseWriter, r *http.Request) {
	var req preparePracticePrintReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.IdempotencyKey == "" {
		writeErr(w, http.StatusBadRequest, "agent / idempotency_key 必填")
		return
	}
	v, replay, err := h.rt.Deps.PreparePracticePrint(r.Context(), req.Agent, r.PathValue("id"),
		req.IdempotencyKey, req.ArtifactKind)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"print_job": toPracticePrintJobDTO(v),
		"replayed":  replay,
	})
}

type prepareGenericPrintReq struct {
	Agent             string `json:"agent"`
	IdempotencyKey    string `json:"idempotency_key"`
	SourceKind        string `json:"source_kind"`
	SourceRef         string `json:"source_ref"`
	Title             string `json:"title"`
	CanonicalMarkdown string `json:"canonical_markdown"`
}

// prepareGenericPrintJob freezes a non-mutating printable Artifact. Query,
// paper, event and retry recovery deliberately share the public /print-jobs
// routes with practice-specific jobs.
func (h *handler) prepareGenericPrintJob(w http.ResponseWriter, r *http.Request) {
	var req prepareGenericPrintReq
	if !decode(w, r, &req) {
		return
	}
	v, replay, err := h.rt.Deps.PrepareGenericPrint(r.Context(), usecase.PrepareGenericPrintRequest{
		AgentName: req.Agent, IdempotencyKey: req.IdempotencyKey, SourceKind: req.SourceKind,
		SourceRef: req.SourceRef, Title: req.Title, CanonicalMarkdown: req.CanonicalMarkdown,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"print_job": toGenericPrintJobDTO(v), "replayed": replay})
}

func isGenericPrintJobID(id string) bool { return strings.HasPrefix(id, "gprint-") }

func (h *handler) getPracticePrintJob(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	if isGenericPrintJobID(jobID) {
		v, err := h.rt.Deps.GetGenericPrint(r.Context(), agent, jobID)
		if err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"print_job": toGenericPrintJobDTO(v)})
		return
	}
	v, err := h.rt.Deps.GetPracticePrint(r.Context(), agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"print_job": toPracticePrintJobDTO(v)})
}

func (h *handler) getPracticePrintJobPaper(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	if isGenericPrintJobID(jobID) {
		v, err := h.rt.Deps.RenderGenericPrintArtifact(r.Context(), agent, jobID)
		if err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"print_job_id": v.PrintJobID, "artifact_id": v.ArtifactID,
			"source_kind": v.SourceKind, "source_ref": v.SourceRef,
			"title": v.Title, "source_digest": v.SourceDigest, "markdown": v.Markdown,
		})
		return
	}
	v, err := h.rt.Deps.RenderPracticePrintJobPaper(r.Context(), agent, jobID, r.URL.Query().Get("kind"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"print_job_id": v.PrintJobID, "kind": v.Kind, "title": v.Title,
		"paper_no": v.PaperNo, "source_digest": v.SourceDigest,
		"artifact_id": v.ArtifactID, "markdown": v.Markdown,
	})
}

type practicePrintEventReq struct {
	Agent           string          `json:"agent"`
	Status          string          `json:"status"`
	NativeJobID     string          `json:"native_job_id,omitempty"`
	NativeReceiptID string          `json:"native_receipt_id,omitempty"`
	PrinterSnapshot json.RawMessage `json:"printer_snapshot,omitempty"`
	FailureKind     string          `json:"failure_kind,omitempty"`
	FailureDetail   string          `json:"failure_detail,omitempty"`
}

type practicePrintCommitReq struct {
	Agent           string          `json:"agent"`
	NativeJobID     string          `json:"native_job_id"`
	NativeReceiptID string          `json:"native_receipt_id"`
	PrinterSnapshot json.RawMessage `json:"printer_snapshot"`
}

// commitPracticePrintReceipt is the single success boundary for both generic
// artifacts and practice baskets. The storage commit is idempotent for the
// exact native receipt; practice baskets additionally finalize the frozen set
// in the same SQLite transaction.
func (h *handler) commitPracticePrintReceipt(w http.ResponseWriter, r *http.Request) {
	var req practicePrintCommitReq
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" || strings.TrimSpace(req.NativeJobID) == "" ||
		strings.TrimSpace(req.NativeReceiptID) == "" || len(req.PrinterSnapshot) == 0 {
		writeErr(w, http.StatusBadRequest, "agent / native_job_id / native_receipt_id / printer_snapshot 必填")
		return
	}
	event := usecase.PracticePrintEvent{
		Status: k12.PrintJobPrinted, NativeJobID: req.NativeJobID,
		NativeReceiptID: req.NativeReceiptID, PrinterSnapshot: string(req.PrinterSnapshot),
	}
	jobID := r.PathValue("id")
	if isGenericPrintJobID(jobID) {
		v, err := h.rt.Deps.RecordGenericPrintEvent(r.Context(), req.Agent, jobID, event)
		if err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"print_job": toGenericPrintJobDTO(v)})
		return
	}
	v, err := h.rt.Deps.RecordPracticePrintEvent(r.Context(), req.Agent, jobID, event)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"print_job": toPracticePrintJobDTO(v)})
}

func (h *handler) recordPracticePrintEvent(w http.ResponseWriter, r *http.Request) {
	var req practicePrintEventReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Status == "" {
		writeErr(w, http.StatusBadRequest, "agent / status 必填")
		return
	}
	event := usecase.PracticePrintEvent{
		Status: req.Status, NativeJobID: req.NativeJobID, NativeReceiptID: req.NativeReceiptID,
		PrinterSnapshot: string(req.PrinterSnapshot), FailureKind: req.FailureKind, FailureDetail: req.FailureDetail,
	}
	jobID := r.PathValue("id")
	if isGenericPrintJobID(jobID) {
		v, err := h.rt.Deps.RecordGenericPrintEvent(r.Context(), req.Agent, jobID, event)
		if err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"print_job": toGenericPrintJobDTO(v)})
		return
	}
	v, err := h.rt.Deps.RecordPracticePrintEvent(r.Context(), req.Agent, jobID, event)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"print_job": toPracticePrintJobDTO(v)})
}

func (h *handler) retryPracticePrintJob(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	jobID := r.PathValue("id")
	if isGenericPrintJobID(jobID) {
		v, err := h.rt.Deps.RetryGenericPrint(r.Context(), req.Agent, jobID)
		if err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"print_job": toGenericPrintJobDTO(v)})
		return
	}
	v, err := h.rt.Deps.RetryPracticePrint(r.Context(), req.Agent, jobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"print_job": toPracticePrintJobDTO(v)})
}

// submitPracticeSet 回传作答（DD-028）：每次必须携带真实照片资产、幂等 return_id 与
// 本批覆盖题目；缺任何字段都拒绝，绝不再把空 item_ids 猜成整卷回传。
type submitReturnReq struct {
	Agent    string   `json:"agent"`
	ReturnID string   `json:"return_id"`
	AssetID  string   `json:"asset_id"`
	ItemIDs  []string `json:"item_ids"`
}

func (h *handler) submitPracticeSet(w http.ResponseWriter, r *http.Request) {
	var req submitReturnReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.ReturnID == "" || req.AssetID == "" || len(req.ItemIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "agent / return_id / asset_id / item_ids 必填")
		return
	}
	v, err := h.rt.Deps.SubmitReturn(r.Context(), req.Agent, r.PathValue("id"), req.ReturnID, req.AssetID, req.ItemIDs)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toPracticeSetDTO(v))
}

// gradeResultsReq 复批命令 body（§3.8 第 3-4 条）：results 为逐题结论；
// 空数组 = 旧行为整卷通过（后向兼容，不联动错题）——新契约请始终携带 results。
type gradeResultsReq struct {
	Agent   string `json:"agent"`
	Results []struct {
		ItemID  string `json:"item_id"`
		Correct bool   `json:"correct"`
	} `json:"results,omitempty"`
}

// gradePracticeSet POST /practice-sets/{id}/grade —— 复批。带 results 走逐题结论链路
// （通过题积掌握证据、未通过题回本周；全部有结论才 graded，允许多次补传覆盖）；
// 空 results 走整卷通过旧行为。
func (h *handler) gradePracticeSet(w http.ResponseWriter, r *http.Request) {
	var req gradeResultsReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	if len(req.Results) == 0 {
		if err := h.rt.Deps.GradePracticeSet(r.Context(), req.Agent, r.PathValue("id")); err != nil {
			writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
			return
		}
		h.respondPracticeSet(w, r, req.Agent)
		return
	}
	results := make([]usecase.PracticeGradeResult, 0, len(req.Results))
	for _, res := range req.Results {
		results = append(results, usecase.PracticeGradeResult{ItemID: res.ItemID, Correct: res.Correct})
	}
	v, err := h.rt.Deps.GradePracticeSetItems(r.Context(), req.Agent, r.PathValue("id"), results)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toPracticeSetDTO(v))
}

func (h *handler) closePracticeSet(w http.ResponseWriter, r *http.Request) {
	h.practiceStep(w, r, (*handler).doClosePractice)
}
func (h *handler) cancelPracticeSet(w http.ResponseWriter, r *http.Request) {
	h.practiceStep(w, r, (*handler).doCancelPractice)
}

func (h *handler) doClosePractice(agent, id string, r *http.Request) error {
	// practiceStep 已消费过 body，这里从 query 取 reason（manual/semester，缺省 manual）。
	return h.rt.Deps.ClosePracticeSet(r.Context(), agent, id, r.URL.Query().Get("reason"))
}
func (h *handler) doCancelPractice(agent, id string, r *http.Request) error {
	return h.rt.Deps.CancelPracticeSet(r.Context(), agent, id)
}

func (h *handler) practiceStep(w http.ResponseWriter, r *http.Request, step func(*handler, string, string, *http.Request) error) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	if err := step(h, req.Agent, r.PathValue("id"), r); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	h.respondPracticeSet(w, r, req.Agent)
}

func (h *handler) respondPracticeSet(w http.ResponseWriter, r *http.Request, agent string) {
	v, err := h.rt.Deps.GetPracticeSet(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toPracticeSetDTO(v))
}
