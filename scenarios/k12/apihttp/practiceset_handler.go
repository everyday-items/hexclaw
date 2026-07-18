package apihttp

import (
	"net/http"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 练习集 DTO（前端契约，PRD §3.8 / §5.5）。
type practiceItemDTO struct {
	ItemID             string `json:"item_id"`
	SourceProblemID    string `json:"source_problem_id,omitempty"`
	Subject            string `json:"subject,omitempty"`
	AddedVia           string `json:"added_via,omitempty"`
	Question           string `json:"question_markdown"`
	ExpectedAnswer     string `json:"expected_answer_markdown,omitempty"`
	VerificationStatus string `json:"verification_status"`
	VerificationBadge  string `json:"verification_evidence,omitempty"`
	BlockedReason      string `json:"blocked_reason,omitempty"`
	PaperSeq           int    `json:"paper_seq,omitempty"`
	Returned           bool   `json:"returned,omitempty"`
	// ResultCorrect 复批逐题结论（§3.8 第 3-4 条）：null=尚无结论。
	ResultCorrect *bool `json:"result_correct,omitempty"`
}

type practiceSetDTO struct {
	RecordID            string            `json:"record_id"`
	Title               string            `json:"title"`
	SourceKind          string            `json:"source_kind"`
	Status              string            `json:"status"`
	StatusLabel         string            `json:"status_label"`
	Publishable         bool              `json:"publishable"`
	QuestionSheet       string            `json:"question_artifact_id,omitempty"`
	AnswerSheet         string            `json:"answer_artifact_id,omitempty"`
	DeliveryStatus      string            `json:"delivery_status"`
	DeliveryTarget      string            `json:"delivery_target,omitempty"`
	SkippedBlockedCount int               `json:"skipped_blocked_count,omitempty"`
	PaperNo             string            `json:"paper_no,omitempty"`
	FinalizedAt         int64             `json:"finalized_at,omitempty"`
	FinalizedVia        string            `json:"finalized_via,omitempty"`
	Items               []practiceItemDTO `json:"items"`
}

func toPracticeSetDTO(v usecase.PracticeSetView) practiceSetDTO {
	items := make([]practiceItemDTO, 0, len(v.Fields.Items))
	for _, it := range v.Fields.Items {
		items = append(items, practiceItemDTO{
			ItemID: it.ItemID, SourceProblemID: it.SourceProblemID, Subject: it.Subject,
			AddedVia: it.AddedVia,
			Question: it.QuestionMarkdown, ExpectedAnswer: it.ExpectedAnswerMarkdown,
			VerificationStatus: it.VerificationStatus, VerificationBadge: it.VerificationEvidence,
			BlockedReason: it.BlockedReason,
			PaperSeq:      it.PaperSeq, Returned: it.Returned, ResultCorrect: it.ResultCorrect,
		})
	}
	return practiceSetDTO{
		RecordID: v.Record.RecordID, Title: v.Fields.Title, SourceKind: v.Fields.SourceKind,
		Status: v.Record.Status, StatusLabel: k12.PracticeSetLabel(v.Record.Status),
		Publishable:         k12.PracticeSetPublishable(v.Fields),
		SkippedBlockedCount: v.Fields.SkippedBlockedCount,
		PaperNo:             v.Fields.PaperNo, FinalizedAt: v.Fields.FinalizedAt, FinalizedVia: v.Fields.FinalizedVia,
		QuestionSheet: v.Fields.QuestionArtifact, AnswerSheet: v.Fields.AnswerArtifact,
		DeliveryStatus: v.Fields.DeliveryStatus, DeliveryTarget: v.Fields.DeliveryTarget,
		Items: items,
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
	Agent  string `json:"agent"`
	Target string `json:"target,omitempty"`
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
	Agent  string `json:"agent"`
	Via    string `json:"via"` // print | send
	Target string `json:"target,omitempty"`
}

func (h *handler) finalizePracticeSet(w http.ResponseWriter, r *http.Request) {
	var req finalizeReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, skipped, err := h.rt.Deps.FinalizeBasket(r.Context(), req.Agent, r.PathValue("id"), req.Via, req.Target)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	resp := map[string]any{"set": toPracticeSetDTO(v), "skipped_blocked_count": skipped}
	// §3.12：send 固化未接真实投递器时 delivery_status=pending，响应注明——不虚标 delivered。
	if v.Fields.DeliveryStatus == k12.PracticeDeliveryPending {
		resp["delivery_note"] = "卷已固化，投递待执行（delivery_status=pending）：投递器未接线，投递结果由渠道适配器回写；渠道失败不回滚已固化的卷"
	}
	writeJSON(w, http.StatusOK, resp)
}

// submitPracticeSet 回传作答：body 可带 item_ids（本次照片覆盖的题，部分回传/补传）；
// 缺省 = 整卷回传。§3.8：全部入卷题回传后才可 graded。
type submitReturnReq struct {
	Agent   string   `json:"agent"`
	ItemIDs []string `json:"item_ids,omitempty"`
}

func (h *handler) submitPracticeSet(w http.ResponseWriter, r *http.Request) {
	var req submitReturnReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	var err error
	if len(req.ItemIDs) > 0 {
		err = h.rt.Deps.SubmitReturn(r.Context(), req.Agent, r.PathValue("id"), req.ItemIDs)
	} else {
		err = h.rt.Deps.SubmitPracticeSet(r.Context(), req.Agent, r.PathValue("id"))
	}
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	h.respondPracticeSet(w, r, req.Agent)
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
	// closed_reason 经 agentOnlyReq.Target 复用会造成语义混淆，close 用独立解码；
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
