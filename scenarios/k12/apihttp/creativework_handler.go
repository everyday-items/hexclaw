package apihttp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type workFeedbackDTO struct {
	FeedbackID         string                         `json:"feedback_id"`
	FeedbackType       string                         `json:"feedback_type"`
	EvidenceRefs       []string                       `json:"evidence_refs"`
	VisibleEvidence    []string                       `json:"visible_evidence"`
	Affirmation        string                         `json:"affirmation"`
	ParentGuidance     string                         `json:"parent_guidance"`
	NextStep           string                         `json:"next_step"`
	SourceSnapshot     k12.WorkFeedbackSourceSnapshot `json:"source_snapshot"`
	Limitations        string                         `json:"limitations,omitempty"`
	ProjectionMarkdown string                         `json:"projection_markdown,omitempty"`
}

type workFeedbackGenerationDTO struct {
	GenerationID   string           `json:"generation_id"`
	Status         string           `json:"status"`
	Feedback       *workFeedbackDTO `json:"feedback,omitempty"`
	FailureMessage string           `json:"failure_message,omitempty"`
}

type creativeWorkDTO struct {
	WorkID          string                     `json:"work_id"`
	WorkType        string                     `json:"work_type"`
	DisplayName     string                     `json:"display_name"`
	WorkTitle       string                     `json:"work_title,omitempty"`
	ContentMarkdown string                     `json:"content_markdown,omitempty"`
	SourceAssetID   string                     `json:"source_asset_id,omitempty"`
	RowVersion      int                        `json:"row_version"`
	InitialFeedback *workFeedbackGenerationDTO `json:"initial_feedback,omitempty"`
	LatestFeedback  *workFeedbackGenerationDTO `json:"latest_feedback,omitempty"`
	DeliveryBatchID string                     `json:"delivery_batch_id,omitempty"`
}

func feedbackGenerationDTO(
	generation *k12.WorkFeedbackGeneration,
) *workFeedbackGenerationDTO {
	if generation == nil {
		return nil
	}
	dto := &workFeedbackGenerationDTO{
		GenerationID:   generation.GenerationID,
		Status:         generation.Status,
		FailureMessage: generation.FailureReason,
	}
	if generation.Feedback == nil {
		return dto
	}
	fact := generation.Feedback
	visible := make([]string, 0, len(fact.Observations))
	for _, observation := range fact.Observations {
		if evidence := strings.TrimSpace(observation.Evidence); evidence != "" {
			visible = append(visible, evidence)
		}
	}
	suggestion := func(index int) string {
		if index < 0 || index >= len(fact.Suggestions) {
			return ""
		}
		return fact.Suggestions[index]
	}
	dto.Feedback = &workFeedbackDTO{
		FeedbackID: fact.FeedbackID, FeedbackType: fact.FeedbackType,
		EvidenceRefs: fact.EvidenceRefs, VisibleEvidence: visible,
		Affirmation: suggestion(0), ParentGuidance: suggestion(1),
		NextStep: suggestion(2), SourceSnapshot: fact.SourceSnapshot,
		Limitations:        fact.Limitations,
		ProjectionMarkdown: fact.ProjectionMarkdown,
	}
	return dto
}

func toCreativeWorkDTO(v usecase.CreativeWorkView) creativeWorkDTO {
	source := k12.CreativeWorkSourceSnapshot{
		WorkType: v.Fields.WorkType, DisplayName: v.Fields.DisplayName,
		WorkTitle: v.Fields.WorkTitle,
	}
	if v.GenerationState.Initial != nil {
		source = v.GenerationState.Initial.Source
	} else if len(v.Fields.Versions) > 0 {
		legacy := v.Fields.Versions[len(v.Fields.Versions)-1]
		source.ContentMarkdown = legacy.ContentMarkdown
		source.SourceAssetID = legacy.SourceAssetID
	}
	displayName := strings.TrimSpace(source.DisplayName)
	if displayName == "" {
		displayName = v.Fields.DisplayName
	}
	workTitle := strings.TrimSpace(source.WorkTitle)
	if workTitle == "" {
		workTitle = v.Fields.WorkTitle
	}
	return creativeWorkDTO{
		WorkID: v.Record.RecordID, WorkType: v.Fields.WorkType,
		DisplayName: displayName, WorkTitle: workTitle,
		ContentMarkdown: source.ContentMarkdown, SourceAssetID: source.SourceAssetID,
		RowVersion:      v.GenerationState.RowVersion,
		InitialFeedback: feedbackGenerationDTO(v.GenerationState.Initial),
		LatestFeedback:  feedbackGenerationDTO(v.GenerationState.Latest),
	}
}

type createWorkReq struct {
	Agent    string `json:"agent"`
	WorkType string `json:"work_type"`
	Content  string `json:"content_markdown"`
}

func (h *handler) createCreativeWork(w http.ResponseWriter, r *http.Request) {
	var req createWorkReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" || req.WorkType != k12.WorkTypeWriting {
		writeErr(w, http.StatusBadRequest, "agent and work_type=writing required")
		return
	}
	commandKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if commandKey == "" {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key required")
		return
	}
	workID, generationID, created, err := h.rt.Deps.CreateCurrentTextWork(
		r.Context(), req.Agent, req.Content, commandKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadRequest), err.Error())
		return
	}
	if h.rt.WorkFeedback != nil {
		h.rt.WorkFeedback.StartAsync(req.Agent, generationID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id": workID, "created": created,
		"initial_feedback_generation_id": generationID,
	})
}

func (h *handler) listCreativeWorks(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ListCreativeWorks(
		r.Context(), agent, r.URL.Query().Get("type"),
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]creativeWorkDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toCreativeWorkDTO(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getCreativeWork(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	view, err := h.rt.Deps.GetCreativeWork(
		r.Context(), agent, r.PathValue("id"),
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(view))
}

func (h *handler) generateWorkFeedback(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	commandKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if req.Agent == "" || commandKey == "" {
		writeErr(w, http.StatusBadRequest, "agent and Idempotency-Key required")
		return
	}
	view, err := h.rt.Deps.GenerateWorkFeedbackCommand(
		r.Context(), req.Agent, r.PathValue("id"), commandKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(view))
}

func (h *handler) sendCreativeWork(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	workID := strings.TrimSpace(r.PathValue("id"))
	view, err := h.rt.Deps.GetCreativeWork(r.Context(), req.Agent, workID)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	content, err := canonicalCreativeWorkText(view)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	batch, _, err := h.rt.Deps.PrepareAndSendTextBatch(
		r.Context(), req.Agent, "creative_work", workID, content,
	)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *handler) deleteCreativeWork(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	expected, commandKey, ok := parseDeleteCommand(w, r)
	if !ok {
		return
	}
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	receipt, err := h.rt.Records.TombstoneCurrentObject(
		r.Context(), agent, "creative_work", r.PathValue("id"),
		expected, commandKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": receipt.Deleted, "work_id": receipt.ObjectID,
		"row_version": receipt.RowVersion,
	})
}

func canonicalCreativeWorkText(view usecase.CreativeWorkView) (string, error) {
	if view.GenerationState.Latest == nil ||
		view.GenerationState.Latest.Feedback == nil {
		return "", fmt.Errorf("%w: 作品尚无成功点评", usecase.ErrInvalidInput)
	}
	dto := toCreativeWorkDTO(view)
	var parts []string
	parts = append(parts, dto.DisplayName)
	if dto.WorkTitle != "" && dto.WorkTitle != dto.DisplayName {
		parts = append(parts, dto.WorkTitle)
	}
	if dto.ContentMarkdown != "" {
		parts = append(parts, dto.ContentMarkdown)
	}
	if dto.SourceAssetID != "" {
		parts = append(parts, "原图："+dto.SourceAssetID)
	}
	parts = append(parts, view.GenerationState.Latest.Feedback.ProjectionMarkdown)
	return strings.Join(parts, "\n\n"), nil
}
