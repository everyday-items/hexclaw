package apihttp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type tutoringTipsSendReq struct {
	Agent   string `json:"agent"`
	Content string `json:"content"`
}

func tutoringTipsObjectID(agentName, content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(agentName) + "\x00" + strings.TrimSpace(content)))
	return "tutoring-tips:" + hex.EncodeToString(sum[:])
}

// sendTutoringTips POST /tutoring-tips/send sends the already rendered inline guidance
// without re-running its model/grounding pipeline. The exact text is frozen in
// the DeliveryBatch and child Receipts before any provider request.
func (h *handler) sendTutoringTips(w http.ResponseWriter, r *http.Request) {
	var req tutoringTipsSendReq
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.Content = strings.TrimSpace(req.Content)
	if req.Agent == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, "agent / content required")
		return
	}
	batch, _, err := h.rt.Deps.PrepareAndSendTextBatch(
		r.Context(), req.Agent, "tutoring_tips", tutoringTipsObjectID(req.Agent, req.Content), req.Content,
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
