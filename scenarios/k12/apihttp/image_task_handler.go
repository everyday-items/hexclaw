package apihttp

import (
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type imageTaskRouteRequest struct {
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	SelectionSource string `json:"selection_source,omitempty"`
}

type createImageTaskReq struct {
	Agent             string                  `json:"agent"`
	SourceSession     string                  `json:"source_session"`
	SourceKind        k12.ImageTaskSourceKind `json:"source_kind"`
	SourceRef         string                  `json:"source_ref"`
	SourceAssetRefs   []string                `json:"source_asset_refs"`
	MessageIntent     string                  `json:"message_intent,omitempty"`
	AttemptGeneration int                     `json:"attempt_generation"`
	RouteRequest      imageTaskRouteRequest   `json:"route_request"`
	CreativeEntry     *struct {
		Kind          k12.CreativeWorkEntryKind `json:"kind"`
		TaskIntent    k12.ImageTaskIntent       `json:"task_intent"`
		WorkID        string                    `json:"work_id,omitempty"`
		BaseVersionID string                    `json:"base_version_id,omitempty"`
	} `json:"creative_entry,omitempty"`
}

type publicImageTaskDispatch struct {
	DispatchID             string                `json:"dispatch_id"`
	TaskIntent             k12.ImageTaskIntent   `json:"task_intent"`
	Status                 k12.ImageTaskStatus   `json:"status"`
	IntentEvidence         []string              `json:"intent_evidence"`
	IntentConfidence       float64               `json:"intent_confidence"`
	ConfirmationCandidates []k12.ImageTaskIntent `json:"confirmation_candidates"`
	Target                 *imageTaskTargetDTO   `json:"target,omitempty"`
	Progress               imageTaskProgressDTO  `json:"progress"`
	TargetProjection       any                   `json:"target_projection,omitempty"`
	Version                int                   `json:"version"`
	CreatedAt              int64                 `json:"created_at"`
	UpdatedAt              int64                 `json:"updated_at"`
}

type imageTaskTargetDTO struct {
	Type k12.ImageTaskTargetType `json:"type"`
	ID   string                  `json:"id"`
}

type imageTaskProgressDTO struct {
	Operation string `json:"operation"`
	State     string `json:"state"`
}

type imageTaskHomeworkProjectionDTO struct {
	Kind              string         `json:"kind"`
	Stage             string         `json:"stage"`
	ConfirmationState string         `json:"confirmation_state"`
	AnchorState       string         `json:"anchor_state"`
	Recognition       map[string]any `json:"recognition,omitempty"`
}

type imageTaskCreativeConflictDTO struct {
	SegmentID     string `json:"segment_id"`
	RawText       string `json:"raw_text,omitempty"`
	CanonicalText string `json:"canonical_text,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type imageTaskCreativeWorkDTO struct {
	WorkID      string `json:"work_id"`
	DisplayName string `json:"display_name"`
}

type imageTaskCreativeProjectionDTO struct {
	Kind              string                          `json:"kind"`
	IntakeID          string                          `json:"intake_id"`
	WorkType          string                          `json:"work_type"`
	Status            k12.CreativeWorkIntakeStatus    `json:"status"`
	EntryKind         k12.CreativeWorkEntryKind       `json:"entry_kind,omitempty"`
	PromotionPolicy   k12.CreativeWorkPromotionPolicy `json:"promotion_policy,omitempty"`
	RoutingProvenance k12.ImageTaskRoutingProvenance  `json:"routing_provenance,omitempty"`
	CommitRequired    *bool                           `json:"commit_required,omitempty"`
	CommitState       string                          `json:"commit_state,omitempty"`
	PromotedWorkID    string                          `json:"promoted_work_id,omitempty"`
	PromotedVersionID string                          `json:"promoted_version_id,omitempty"`
	CanonicalVersion  int                             `json:"canonical_version,omitempty"`
	CanonicalContent  string                          `json:"canonical_content,omitempty"`
	Conflicts         []imageTaskCreativeConflictDTO  `json:"conflicts,omitempty"`
	Work              *imageTaskCreativeWorkDTO       `json:"work,omitempty"`
}

func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func nonNilIntents(values []k12.ImageTaskIntent) []k12.ImageTaskIntent {
	if len(values) == 0 {
		return []k12.ImageTaskIntent{}
	}
	return append([]k12.ImageTaskIntent(nil), values...)
}

func publicImageTaskProgressState(state string) string {
	switch strings.TrimSpace(state) {
	case "outcome_unknown", "feedback_outcome_unknown":
		return "recovering"
	default:
		return state
	}
}

func publicImageTask(view usecase.ImageTaskView) publicImageTaskDispatch {
	dispatch := view.Dispatch
	out := publicImageTaskDispatch{
		DispatchID: dispatch.DispatchID, TaskIntent: dispatch.TaskIntent, Status: dispatch.Status,
		IntentEvidence:         nonNilStrings(dispatch.IntentEvidence),
		IntentConfidence:       dispatch.IntentConfidence,
		ConfirmationCandidates: nonNilIntents(dispatch.ConfirmationCandidates),
		Progress: imageTaskProgressDTO{
			Operation: "classification", State: string(dispatch.Status),
		},
		Version:   dispatch.Version,
		CreatedAt: dispatch.CreatedAt, UpdatedAt: dispatch.UpdatedAt,
	}
	if dispatch.TargetObjectType != "" && dispatch.TargetObjectID != "" {
		out.Target = &imageTaskTargetDTO{
			Type: dispatch.TargetObjectType,
			ID:   dispatch.TargetObjectID,
		}
	}
	if view.Homework != nil {
		projection := imageTaskHomeworkProjectionDTO{
			Kind: "homework", Stage: "queued",
			ConfirmationState: "pending", AnchorState: "pending",
		}
		if view.Homework.Status == k12.HomeworkSubmissionCancelled {
			projection.Stage = "cancelled"
		}
		if view.Homework.Status == k12.HomeworkSubmissionFailed {
			projection.Stage = "failed_terminal"
		}
		if view.HomeworkProjection != nil {
			questions := make([]recognizedQuestionDTO, 0, len(view.HomeworkProjection.Questions))
			for _, question := range view.HomeworkProjection.Questions {
				questions = append(questions, recognizedQuestionToDTO(question, true))
			}
			projection.Stage = publicImageTaskProgressState(view.HomeworkProjection.Stage)
			projection.ConfirmationState = view.HomeworkProjection.ConfirmationState
			projection.AnchorState = view.HomeworkProjection.AnchorState
			projection.Recognition = map[string]any{
				"subject": view.HomeworkProjection.Subject, "questions": questions,
			}
		}
		out.Progress = imageTaskProgressDTO{Operation: "homework", State: projection.Stage}
		out.TargetProjection = projection
	}
	if view.Creative != nil {
		entryKind := view.Creative.EntryKind
		if entryKind == "" {
			entryKind = k12.CreativeWorkEntryAuto
		}
		promotionPolicy := view.Creative.PromotionPolicy
		if promotionPolicy == "" {
			promotionPolicy = k12.CreativeWorkPromotionAutomatic
		}
		projection := imageTaskCreativeProjectionDTO{
			Kind: "creative", IntakeID: view.Creative.IntakeID,
			WorkType: view.Creative.WorkType, Status: view.Creative.Status,
			EntryKind: entryKind, PromotionPolicy: promotionPolicy,
			RoutingProvenance: dispatch.RoutingProvenance,
			PromotedWorkID:    view.Creative.PromotedWorkID,
			PromotedVersionID: view.Creative.PromotedVersionID,
		}
		if projection.RoutingProvenance == "" {
			projection.RoutingProvenance = k12.ImageTaskRoutingModelClassified
		}
		if promotionPolicy == k12.CreativeWorkPromotionExplicitCommit {
			required := view.Creative.Status != k12.CreativeWorkIntakePromoted
			projection.CommitRequired = &required
			projection.CommitState = "pending"
			if !required {
				projection.CommitState = "committed"
			}
		}
		if view.Creative.OCREvidence != nil {
			projection.CanonicalVersion = view.Creative.OCREvidence.CanonicalVersion
			projection.CanonicalContent = view.Creative.OCREvidence.CanonicalContent
			if len(view.Creative.OCREvidence.RiskSegments) > 0 {
				projection.Conflicts = make(
					[]imageTaskCreativeConflictDTO, 0, len(view.Creative.OCREvidence.RiskSegments),
				)
				for _, risk := range view.Creative.OCREvidence.RiskSegments {
					projection.Conflicts = append(projection.Conflicts, imageTaskCreativeConflictDTO{
						SegmentID: risk.SegmentID,
						RawText:   risk.RawText,
						Reason:    strings.Join(risk.Reasons, "; "),
					})
				}
			}
		}
		if view.Creative.PromotedWorkID != "" {
			projection.Work = &imageTaskCreativeWorkDTO{
				WorkID: view.Creative.PromotedWorkID, DisplayName: view.CreativeDisplayName,
			}
		}
		operation := "writing_ocr"
		if view.Creative.WorkType == k12.WorkTypeArt ||
			view.Creative.Status == k12.CreativeWorkIntakeReady ||
			view.Creative.Status == k12.CreativeWorkIntakePromoted {
			operation = "promotion"
		}
		out.Progress = imageTaskProgressDTO{
			Operation: operation, State: string(view.Creative.Status),
		}
		if view.Creative.Status == k12.CreativeWorkIntakePromoted &&
			view.CreativeFeedback != "" {
			out.Progress.State = publicImageTaskProgressState(view.CreativeFeedback)
		}
		out.TargetProjection = projection
	}
	return out
}

func (h *handler) createImageTask(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	var req createImageTaskReq
	if !decodeStrict(w, r, &req) {
		return
	}
	input := usecase.CreateImageTaskInput{
		AgentName: req.Agent, LearnerID: req.Agent,
		SourceKind: req.SourceKind, SourceRef: req.SourceRef,
		SourceSessionID: req.SourceSession, SourceAssetRefs: req.SourceAssetRefs,
		MessageIntent: req.MessageIntent, AttemptGeneration: req.AttemptGeneration,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: req.RouteRequest.Provider, Model: req.RouteRequest.Model,
			SelectionSource: req.RouteRequest.SelectionSource,
		},
	}
	if req.CreativeEntry != nil {
		input.CreativeEntry = &k12.ImageTaskCreativeEntry{
			Kind: req.CreativeEntry.Kind, TaskIntent: req.CreativeEntry.TaskIntent,
			WorkID: req.CreativeEntry.WorkID, BaseVersionID: req.CreativeEntry.BaseVersionID,
		}
	}
	view, created, err := h.rt.ImageTasks.Create(r.Context(), input)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadGateway), err.Error())
		return
	}
	h.rt.ImageTasks.StartAsync(req.Agent, view.Dispatch.DispatchID)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": created, "dispatch": publicImageTask(view),
	})
}

func imageTaskAgent(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("agent"))
}

func (h *handler) getImageTask(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	agent := imageTaskAgent(r)
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	view, err := h.rt.ImageTasks.Get(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatch": publicImageTask(view)})
}

type confirmImageTaskReq struct {
	Agent    string              `json:"agent"`
	Version  int                 `json:"version"`
	Intent   k12.ImageTaskIntent `json:"intent,omitempty"`
	Homework *struct {
		Subject             string                              `json:"subject,omitempty"`
		Grade               string                              `json:"grade,omitempty"`
		QuestionCorrections []usecase.GradingQuestionCorrection `json:"question_corrections,omitempty"`
	} `json:"homework,omitempty"`
	Creative *struct {
		Action             usecase.CreativeImageTaskAction       `json:"action"`
		CanonicalVersion   int                                   `json:"canonical_version"`
		CanonicalContent   string                                `json:"canonical_content,omitempty"`
		WorkTitle          string                                `json:"work_title,omitempty"`
		TaskRequirement    string                                `json:"task_requirement,omitempty"`
		Intent             string                                `json:"intent,omitempty"`
		ContentMarkdown    string                                `json:"content_markdown,omitempty"`
		SegmentCorrections []k12.CreativeWorkIntakeOCRCorrection `json:"segment_corrections,omitempty"`
	} `json:"creative,omitempty"`
}

func (h *handler) confirmImageTask(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	var req confirmImageTaskReq
	if !decodeStrict(w, r, &req) {
		return
	}
	input := usecase.ConfirmImageTaskInput{
		AgentName: req.Agent, DispatchID: r.PathValue("id"),
		ExpectedVersion: req.Version, Intent: req.Intent,
	}
	if req.Homework != nil {
		input.Subject, input.Grade = req.Homework.Subject, req.Homework.Grade
		input.QuestionCorrections = req.Homework.QuestionCorrections
	}
	if req.Creative != nil {
		if req.Homework != nil || req.Intent != "" {
			writeErr(w, http.StatusBadRequest, "creative/homework/intent confirmation branches are exclusive")
			return
		}
		switch req.Creative.Action {
		case usecase.CreativeImageTaskActionFreezeOCR:
			if strings.TrimSpace(req.Creative.WorkTitle) != "" ||
				strings.TrimSpace(req.Creative.TaskRequirement) != "" ||
				strings.TrimSpace(req.Creative.Intent) != "" ||
				strings.TrimSpace(req.Creative.ContentMarkdown) != "" {
				writeErr(w, http.StatusBadRequest, "freeze_ocr cannot carry commit fields")
				return
			}
		case usecase.CreativeImageTaskActionCommit:
			if req.Creative.CanonicalVersion != 0 ||
				strings.TrimSpace(req.Creative.CanonicalContent) != "" ||
				len(req.Creative.SegmentCorrections) != 0 {
				writeErr(w, http.StatusBadRequest, "commit cannot carry freeze_ocr fields")
				return
			}
		default:
			writeErr(w, http.StatusBadRequest, "creative.action must be freeze_ocr or commit")
			return
		}
		if strings.TrimSpace(req.Creative.CanonicalContent) == "" &&
			len(req.Creative.SegmentCorrections) > 0 {
			writeErr(w, http.StatusBadRequest,
				"segment_corrections require canonical_content to freeze an unambiguous full version")
			return
		}
		input.Creative = &usecase.ConfirmCreativeImageTaskInput{
			Action:           req.Creative.Action,
			CanonicalVersion: req.Creative.CanonicalVersion,
			CanonicalContent: req.Creative.CanonicalContent,
			SegmentCorrections: append(
				[]k12.CreativeWorkIntakeOCRCorrection(nil),
				req.Creative.SegmentCorrections...,
			),
			WorkTitle:       req.Creative.WorkTitle,
			TaskRequirement: req.Creative.TaskRequirement,
			Intent:          req.Creative.Intent,
			ContentMarkdown: req.Creative.ContentMarkdown,
		}
	}
	view, err := h.rt.ImageTasks.Confirm(r.Context(), input)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatch": publicImageTask(view)})
}

type imageTaskVersionReq struct {
	Agent   string `json:"agent"`
	Version int    `json:"version"`
}

func (h *handler) retryImageTask(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	var req imageTaskVersionReq
	if !decodeStrict(w, r, &req) {
		return
	}
	view, err := h.rt.ImageTasks.Retry(r.Context(), req.Agent, r.PathValue("id"), req.Version)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatch": publicImageTask(view)})
}

func (h *handler) cancelImageTask(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	var req imageTaskVersionReq
	if !decodeStrict(w, r, &req) {
		return
	}
	view, err := h.rt.ImageTasks.Cancel(r.Context(), req.Agent, r.PathValue("id"), req.Version)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatch": publicImageTask(view)})
}

func (h *handler) getImageTaskResult(w http.ResponseWriter, r *http.Request) {
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	agent := imageTaskAgent(r)
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	result, err := h.rt.ImageTasks.Result(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	var projection any
	if result.Photo != nil {
		projection = map[string]any{
			"kind":    string(result.Dispatch.TaskIntent),
			"payload": photoResultDTO(*result.Photo),
		}
	} else if result.Kind == "creative" && result.Creative != nil {
		payload := map[string]any{
			"intake": map[string]any{
				"intake_id": result.Creative.IntakeID,
				"status":    result.Creative.Status,
			},
		}
		if result.Creative.PromotedWorkID != "" {
			payload["work"] = map[string]any{
				"work_id":      result.Creative.PromotedWorkID,
				"display_name": result.CreativeDisplayName,
			}
		}
		if result.CreativeWork != nil {
			var feedback *k12.WorkFeedback
			if latest := result.CreativeWork.GenerationState.Latest; latest != nil &&
				latest.Status == k12.WorkFeedbackSucceeded {
				feedback = latest.Feedback
			} else if len(result.CreativeWork.Fields.Versions) > 0 {
				version := result.CreativeWork.Fields.Versions[len(result.CreativeWork.Fields.Versions)-1]
				feedback = version.StructuredFeedback
			}
			if feedback != nil && feedback.ProjectionMarkdown != "" {
				payload["feedback"] = map[string]any{
					"structured_feedback": feedback,
					"projection_markdown": feedback.ProjectionMarkdown,
				}
			}
		}
		projection = map[string]any{
			"kind": string(result.Dispatch.TaskIntent), "payload": payload,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dispatch_id": result.Dispatch.DispatchID,
		"task_intent": result.Dispatch.TaskIntent,
		"status":      result.Dispatch.Status,
		"result":      projection,
	})
}
