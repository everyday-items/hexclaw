package apihttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
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
	WorkID             string                     `json:"work_id"`
	WorkType           string                     `json:"work_type"`
	DisplayName        string                     `json:"display_name"`
	WorkTitle          string                     `json:"work_title,omitempty"`
	ContentMarkdown    string                     `json:"content_markdown,omitempty"`
	SourceAssetID      string                     `json:"source_asset_id,omitempty"`
	RowVersion         int                        `json:"row_version"`
	InitialFeedback    *workFeedbackGenerationDTO `json:"initial_feedback,omitempty"`
	LatestFeedback     *workFeedbackGenerationDTO `json:"latest_feedback,omitempty"`
	DeliveryBatchID    string                     `json:"delivery_batch_id,omitempty"`
	CreatedAt          int64                      `json:"created_at"`
	LatestGenerationAt *int64                     `json:"latest_generation_at"`
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
		Affirmation: fact.Affirmation, ParentGuidance: fact.ParentGuidance,
		NextStep: fact.NextStep, SourceSnapshot: fact.SourceSnapshot,
		Limitations:        fact.Limitations,
		ProjectionMarkdown: fact.ProjectionMarkdown,
	}
	if fact.Affirmation == "" && fact.ParentGuidance == "" && fact.NextStep == "" {
		// 仅为缺少语义字段的旧记录保留原 DTO 读取兼容。
		dto.Feedback.Affirmation = suggestion(0)
		dto.Feedback.ParentGuidance = suggestion(1)
		dto.Feedback.NextStep = suggestion(2)
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
	var latestGenerationAt *int64
	if latest := v.GenerationState.Latest; latest != nil && latest.Status == k12.WorkFeedbackSucceeded {
		completedAt := latest.UpdatedAt
		latestGenerationAt = &completedAt
	} else if initial := v.GenerationState.Initial; initial != nil && initial.Status == k12.WorkFeedbackSucceeded {
		completedAt := initial.UpdatedAt
		latestGenerationAt = &completedAt
	}
	return creativeWorkDTO{
		WorkID: v.Record.RecordID, WorkType: v.Fields.WorkType,
		DisplayName: displayName, WorkTitle: workTitle,
		ContentMarkdown: source.ContentMarkdown, SourceAssetID: source.SourceAssetID,
		RowVersion:         v.GenerationState.RowVersion,
		InitialFeedback:    feedbackGenerationDTO(v.GenerationState.Initial),
		LatestFeedback:     feedbackGenerationDTO(v.GenerationState.Latest),
		CreatedAt:          v.Record.CreatedAt,
		LatestGenerationAt: latestGenerationAt,
	}
}

func (h *handler) creativeWorkDTOWithDeliveryBatch(
	ctx context.Context,
	agent string,
	view usecase.CreativeWorkView,
) (creativeWorkDTO, error) {
	dto := toCreativeWorkDTO(view)
	if view.GenerationState.Latest == nil || view.GenerationState.Latest.Feedback == nil {
		return dto, nil
	}
	content, err := canonicalCreativeWorkText(view)
	if err != nil {
		return creativeWorkDTO{}, err
	}
	var attachments []usecase.DeliveryAttachmentIdentity
	if strings.TrimSpace(dto.SourceAssetID) != "" {
		identity, ok := creativeWorkAttachmentIdentity(agent, dto)
		if !ok {
			return dto, nil
		}
		attachments = []usecase.DeliveryAttachmentIdentity{identity}
	}
	batch, err := h.rt.Deps.GetDeliveryBatchForMessageIdentity(
		ctx, agent, "creative_work", dto.WorkID, content, attachments,
	)
	if errors.Is(err, records.ErrNotFound) {
		return dto, nil
	}
	if err != nil {
		return creativeWorkDTO{}, err
	}
	dto.DeliveryBatchID = batch.BatchID
	return dto, nil
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
		dto, dtoErr := h.creativeWorkDTOWithDeliveryBatch(r.Context(), agent, item)
		if dtoErr != nil {
			writeErr(w, http.StatusInternalServerError, dtoErr.Error())
			return
		}
		out = append(out, dto)
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
	dto, err := h.creativeWorkDTOWithDeliveryBatch(r.Context(), agent, view)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
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
	dto := toCreativeWorkDTO(view)
	sourceAssetID := strings.TrimSpace(dto.SourceAssetID)
	ownerScope := ""
	if sourceAssetID != "" {
		ownerScope, err = h.authorizedAgentOwnerScope(r.Context(), req.Agent)
		if err != nil {
			writeAssetScopeErr(w, err)
			return
		}
	}
	content, err := canonicalCreativeWorkText(view)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	message := usecase.DeliveryMessage{Content: content}
	if sourceAssetID != "" {
		identity, hasIdentity := creativeWorkAttachmentIdentity(req.Agent, dto)
		replayExisting := func() (k12.DeliveryBatch, bool, error) {
			if !hasIdentity {
				return k12.DeliveryBatch{}, false, nil
			}
			existing, lookupErr := h.rt.Deps.ReplayDeliveryBatchForMessageIdentity(
				r.Context(), req.Agent, "creative_work", workID, content,
				[]usecase.DeliveryAttachmentIdentity{identity},
			)
			if errors.Is(lookupErr, records.ErrNotFound) {
				return k12.DeliveryBatch{}, false, nil
			}
			return existing, lookupErr == nil, lookupErr
		}
		if existing, found, replayErr := replayExisting(); replayErr != nil {
			writeDeliveryError(w, replayErr)
			return
		} else if found {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		gateway := h.pageAssetGateway()
		if gateway == nil {
			writeErr(w, http.StatusServiceUnavailable, "PageAsset repository unavailable")
			return
		}
		ready, openErr := gateway.OpenReady(
			r.Context(), ownerScope, req.Agent, sourceAssetID,
		)
		if openErr != nil {
			if existing, found, replayErr := replayExisting(); replayErr != nil {
				writeDeliveryError(w, replayErr)
				return
			} else if found {
				writeJSON(w, http.StatusOK, existing)
				return
			}
			if errors.Is(openErr, records.ErrScopeNotFound) ||
				errors.Is(openErr, k12storage.ErrPageAssetNotFound) {
				writeErr(w, http.StatusNotFound, "Creative work asset not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "Creative work asset unavailable")
			return
		}
		extension := ""
		switch ready.Metadata.MediaType {
		case "image/png":
			extension = ".png"
		case "image/jpeg":
			extension = ".jpg"
		case "image/gif":
			extension = ".gif"
		case "image/webp":
			extension = ".webp"
		default:
			writeErr(w, http.StatusInternalServerError, "Creative work asset media type unavailable")
			return
		}
		message.Attachments = []usecase.DeliveryAttachment{{
			Name: dto.DisplayName + extension,
			MIME: ready.Metadata.MediaType,
			Data: append([]byte(nil), ready.Data...),
		}}
	}
	batch, _, err := h.rt.Deps.PrepareAndSendMessageBatch(
		r.Context(), req.Agent, "creative_work", workID, message,
	)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func creativeWorkAttachmentIdentity(
	agent string,
	dto creativeWorkDTO,
) (usecase.DeliveryAttachmentIdentity, bool) {
	assetAgent, file, err := assetstore.Parse(dto.SourceAssetID)
	if err != nil || assetAgent != strings.TrimSpace(agent) {
		return usecase.DeliveryAttachmentIdentity{}, false
	}
	dot := strings.LastIndexByte(file, '.')
	if dot <= 0 {
		return usecase.DeliveryAttachmentIdentity{}, false
	}
	extension := file[dot:]
	mediaType := ""
	switch extension {
	case ".png":
		mediaType = "image/png"
	case ".jpg":
		mediaType = "image/jpeg"
	case ".gif":
		mediaType = "image/gif"
	case ".webp":
		mediaType = "image/webp"
	default:
		return usecase.DeliveryAttachmentIdentity{}, false
	}
	return usecase.DeliveryAttachmentIdentity{
		Name:          dto.DisplayName + extension,
		MIME:          mediaType,
		ContentDigest: "sha256:" + file[:dot],
	}, true
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
	parts = append(parts, view.GenerationState.Latest.Feedback.ProjectionMarkdown)
	return strings.Join(parts, "\n\n"), nil
}
