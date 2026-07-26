package apihttp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type tutoringTipsSendReq struct {
	Agent           string `json:"agent"`
	FinalArtifactID string `json:"final_artifact_id"`
}

// sendTutoringTips sends only the canonical frozen grading final_artifact.
// The client cannot inject page content.
func (h *handler) sendTutoringTips(w http.ResponseWriter, r *http.Request) {
	var req tutoringTipsSendReq
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.FinalArtifactID = strings.TrimSpace(req.FinalArtifactID)
	if req.Agent == "" || req.FinalArtifactID == "" {
		writeErr(w, http.StatusBadRequest, "agent / final_artifact_id required")
		return
	}
	batch, _, err := h.rt.Deps.PrepareAndSendGradingFinalArtifact(
		r.Context(), req.Agent, req.FinalArtifactID,
	)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *handler) getDeliveryBatch(w http.ResponseWriter, r *http.Request) {
	agentName := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agentName == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	batch, err := h.rt.Deps.GetDeliveryBatch(r.Context(), agentName, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *handler) retryDeliveryBatch(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	batch, err := h.rt.Deps.RetryDeliveryBatch(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *handler) queryDeliveryBatch(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	batch, err := h.rt.Deps.QueryDeliveryBatch(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *handler) getDeliveryReceipt(w http.ResponseWriter, r *http.Request) {
	agentName := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agentName == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	receipt, err := h.rt.Deps.GetDeliveryReceipt(r.Context(), agentName, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (h *handler) retryDeliveryReceipt(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	receipt, err := h.rt.Deps.RetryDeliveryReceipt(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (h *handler) queryDeliveryReceipt(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	receipt, err := h.rt.Deps.QueryDeliveryReceipt(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func writeDeliveryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usecase.ErrDeliveryUnavailable):
		writeErr(w, http.StatusNotImplemented, "发送到手机还没有开通，请先在连接设置里完成私聊绑定")
	case errors.Is(err, usecase.ErrNoActiveDirectBindings):
		writeErr(w, http.StatusConflict, "这个辅导助手还没绑定手机私聊：先在连接设置里绑定")
	case errors.Is(err, usecase.ErrDeliveryQueryUnavailable), errors.Is(err, records.ErrIllegalTransition):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
	}
}
