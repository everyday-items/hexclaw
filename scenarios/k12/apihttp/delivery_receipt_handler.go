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

type prepCardSendReq struct {
	Agent   string `json:"agent"`
	Content string `json:"content"`
}

func prepCardObjectID(agentName, content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(agentName) + "\x00" + strings.TrimSpace(content)))
	return "prep-card:" + hex.EncodeToString(sum[:])
}

// sendPrepCard POST /prep-card/send sends the already rendered in-session card
// without re-running its model/grounding pipeline. The exact text is frozen in
// the Receipt before any provider request.
func (h *handler) sendPrepCard(w http.ResponseWriter, r *http.Request) {
	var req prepCardSendReq
	if !decode(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.Content = strings.TrimSpace(req.Content)
	if req.Agent == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, "agent / content required")
		return
	}
	receipt, _, err := h.rt.Deps.PrepareAndSendText(
		r.Context(), req.Agent, "prep_card", prepCardObjectID(req.Agent, req.Content), req.Content,
	)
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
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
	case errors.Is(err, usecase.ErrDeliveryQueryUnavailable), errors.Is(err, records.ErrIllegalTransition):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
	}
}
