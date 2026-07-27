package apihttp

import (
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type practiceCandidateOpenReq struct {
	Agent          string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
	Grade          string `json:"grade,omitempty"`
	Textbook       string `json:"textbook,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
}

func (h *handler) openPracticeCandidateSelection(w http.ResponseWriter, r *http.Request) {
	var req practiceCandidateOpenReq
	if !decodeStrict(w, r, &req) {
		return
	}
	selection, err := h.rt.Deps.OpenPracticeCandidateSelection(
		r.Context(), req.Agent, r.PathValue("record_id"),
		usecase.PracticeCandidateSelectionRequest{
			IdempotencyKey: req.IdempotencyKey,
			Grade:          req.Grade,
			Textbook:       req.Textbook,
			Provider:       req.Provider,
			Model:          req.Model,
			SourceSession:  req.SourceSession,
		},
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, selection)
}

type practiceCandidateBatchReq struct {
	Agent          string `json:"agent"`
	Revision       *int   `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
}

func (h *handler) generatePracticeCandidateBatch(w http.ResponseWriter, r *http.Request) {
	var req practiceCandidateBatchReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Revision == nil {
		writeErr(w, http.StatusBadRequest, "revision required")
		return
	}
	selection, err := h.rt.Deps.GeneratePracticeCandidateBatch(
		r.Context(), req.Agent, r.PathValue("id"),
		usecase.PracticeCandidateBatchRequest{
			Revision:       *req.Revision,
			IdempotencyKey: req.IdempotencyKey,
			Provider:       req.Provider,
			Model:          req.Model,
		},
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, selection)
}

type practiceCandidateCommitReq struct {
	Agent          string   `json:"agent"`
	Revision       *int     `json:"revision"`
	CandidateIDs   []string `json:"candidate_ids"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (h *handler) commitPracticeCandidateSelection(w http.ResponseWriter, r *http.Request) {
	var req practiceCandidateCommitReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Revision == nil {
		writeErr(w, http.StatusBadRequest, "revision required")
		return
	}
	receipt, err := h.rt.Deps.CommitPracticeCandidateSelection(
		r.Context(), k12storage.PracticeCandidateCommitInput{
			AgentName:      req.Agent,
			SelectionID:    r.PathValue("id"),
			Revision:       *req.Revision,
			CandidateIDs:   req.CandidateIDs,
			IdempotencyKey: req.IdempotencyKey,
		},
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

type mistakeReviewCommandReq struct {
	Agent          string `json:"agent"`
	Version        *int   `json:"version"`
	IdempotencyKey string `json:"idempotency_key"`
	PlanID         string `json:"plan_id,omitempty"`
	PlanRevision   int    `json:"plan_revision,omitempty"`
	WeeklyItemID   string `json:"weekly_item_id,omitempty"`
	ISOYear        int    `json:"iso_year,omitempty"`
	ISOWeek        int    `json:"iso_week,omitempty"`
}

func (h *handler) suppressMistakeReview(w http.ResponseWriter, r *http.Request) {
	h.applyMistakeReviewCommand(w, r, k12.MistakeReviewCommandSuppress, false)
}

func (h *handler) restoreMistakeReview(w http.ResponseWriter, r *http.Request) {
	h.applyMistakeReviewCommand(w, r, k12.MistakeReviewCommandRestore, false)
}

func (h *handler) deferMistakeThisWeek(w http.ResponseWriter, r *http.Request) {
	h.applyMistakeReviewCommand(w, r, k12.MistakeReviewCommandDeferThisWeek, true)
}

func (h *handler) applyMistakeReviewCommand(
	w http.ResponseWriter,
	r *http.Request,
	commandType string,
	deferCommand bool,
) {
	var req mistakeReviewCommandReq
	if !decodeStrict(w, r, &req) {
		return
	}
	recordID := strings.TrimSpace(r.PathValue("record_id"))
	if req.Version == nil || req.Agent == "" || recordID == "" ||
		req.IdempotencyKey == "" {
		writeErr(w, http.StatusBadRequest,
			"agent/record_id/version/idempotency_key required")
		return
	}
	if deferCommand && (req.ISOYear <= 0 || req.ISOWeek < 1 || req.ISOWeek > 53) {
		writeErr(w, http.StatusBadRequest, "valid iso_year/iso_week required")
		return
	}
	result, err := h.rt.Deps.Records.ApplyMistakeReviewCommand(
		r.Context(), k12storage.MistakeReviewCommandInput{
			AgentName: req.Agent, MistakeRecordID: recordID,
			ExpectedVersion: *req.Version, IdempotencyKey: req.IdempotencyKey,
			CommandType: commandType, ISOYear: req.ISOYear, ISOWeek: req.ISOWeek,
			PlanID: req.PlanID, PlanRevision: req.PlanRevision,
			WeeklyItemID: req.WeeklyItemID,
		},
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	if deferCommand {
		writeJSON(w, http.StatusOK, result)
		return
	}
	record, err := h.rt.Deps.Records.Get(r.Context(), recordID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	fields, err := k12.ParseMistakeFields(record.Fields)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mistakeDTOWithReview(record, fields, result.Review))
}
