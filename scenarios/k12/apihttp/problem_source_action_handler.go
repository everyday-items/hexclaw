package apihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type problemSourceActionRequest struct {
	Action                string          `json:"action"`
	StructureVersion      int             `json:"structure_version"`
	ExpectedInputRevision int             `json:"expected_input_revision"`
	Payload               json.RawMessage `json:"payload"`
}

type correctTextSourceActionPayload struct {
	QuestionCanonicalMarkdown string `json:"question_canonical_markdown"`
	AnswerCanonicalMarkdown   string `json:"answer_canonical_markdown"`
}

type selectRegionSourceActionPayload struct {
	PageAssetID string `json:"page_asset_id"`
	Region      struct {
		X      int `json:"x"`
		Y      int `json:"y"`
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"region"`
}

type retakeSourceActionPayload struct {
	PageAssetID string `json:"page_asset_id"`
}

func (h *handler) problemSourceAction(w http.ResponseWriter, r *http.Request) {
	dispatchID := strings.TrimSpace(r.PathValue("dispatch_id"))
	problemID := strings.TrimSpace(r.PathValue("problem_id"))
	if dispatchID == "" || problemID == "" {
		writeErr(w, http.StatusBadRequest, "dispatch_id and problem_id required")
		return
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key required")
		return
	}

	var req problemSourceActionRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	if req.StructureVersion <= 0 || req.ExpectedInputRevision <= 0 {
		writeErr(w, http.StatusBadRequest, "positive structure_version and expected_input_revision required")
		return
	}
	if !validateProblemSourceActionPayload(w, req) {
		return
	}

	ownerScope, err := h.problemSourceActionOwnerScope(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authenticated image task principal required")
		return
	}
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task coordinator unavailable")
		return
	}
	result, err := h.rt.ImageTasks.CommitProblemSourceAction(
		r.Context(),
		usecase.ProblemSourceActionCommand{
			OwnerScope:            ownerScope,
			TrustedAgentName:      problemSourceActionTrustedAgent(h.rt, ownerScope),
			DispatchID:            dispatchID,
			ProblemID:             problemID,
			IdempotencyKey:        strings.TrimSpace(r.Header.Get("Idempotency-Key")),
			Action:                req.Action,
			StructureVersion:      req.StructureVersion,
			ExpectedInputRevision: req.ExpectedInputRevision,
			Payload:               req.Payload,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, k12storage.ErrProblemSourceActionNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, k12storage.ErrProblemSourceActionConflict):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func problemSourceActionTrustedAgent(rt Runtime, ownerScope string) string {
	if strings.TrimSpace(rt.PrincipalMode) == "remote" {
		return strings.TrimSpace(ownerScope)
	}
	return ""
}

func (h *handler) problemSourceActionOwnerScope(
	ctx context.Context,
) (string, error) {
	switch strings.TrimSpace(h.rt.PrincipalMode) {
	case "local_loopback":
		ownerScope := strings.TrimSpace(h.rt.OwnerScope)
		if ownerScope == "" {
			return "", errors.New("local owner scope missing")
		}
		return ownerScope, nil
	case "remote":
		if h.rt.AuthenticatedOwnerScope == nil {
			return "", errors.New("remote principal resolver missing")
		}
		ownerScope, err := h.rt.AuthenticatedOwnerScope(ctx)
		if err != nil || strings.TrimSpace(ownerScope) == "" {
			return "", errors.New("authenticated owner scope missing")
		}
		return strings.TrimSpace(ownerScope), nil
	default:
		return "", errors.New("unsupported principal mode")
	}
}

func validateProblemSourceActionPayload(w http.ResponseWriter, req problemSourceActionRequest) bool {
	switch req.Action {
	case "correct_text":
		var payload correctTextSourceActionPayload
		if !decodeProblemSourceActionPayload(w, req.Payload, &payload) {
			return false
		}
		if strings.TrimSpace(payload.QuestionCanonicalMarkdown) == "" &&
			strings.TrimSpace(payload.AnswerCanonicalMarkdown) == "" {
			writeErr(w, http.StatusUnprocessableEntity, "correct_text requires question or answer canonical markdown")
			return false
		}
	case "select_region":
		var payload selectRegionSourceActionPayload
		if !decodeProblemSourceActionPayload(w, req.Payload, &payload) {
			return false
		}
		if strings.TrimSpace(payload.PageAssetID) == "" ||
			payload.Region.X < 0 || payload.Region.Y < 0 ||
			payload.Region.Width <= 0 || payload.Region.Height <= 0 {
			writeErr(w, http.StatusUnprocessableEntity, "select_region requires page_asset_id and a positive source-pixel region")
			return false
		}
	case "retake":
		var payload retakeSourceActionPayload
		if !decodeProblemSourceActionPayload(w, req.Payload, &payload) {
			return false
		}
		if strings.TrimSpace(payload.PageAssetID) == "" {
			writeErr(w, http.StatusUnprocessableEntity, "retake requires page_asset_id")
			return false
		}
	case "skip", "resume":
		var payload struct{}
		if !decodeProblemSourceActionPayload(w, req.Payload, &payload) {
			return false
		}
	default:
		writeErr(w, http.StatusBadRequest, "unsupported source action")
		return false
	}
	return true
}

func decodeProblemSourceActionPayload(w http.ResponseWriter, raw json.RawMessage, dst any) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "action payload required")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid action payload: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, http.StatusUnprocessableEntity, "action payload must contain exactly one JSON value")
		return false
	}
	return true
}
