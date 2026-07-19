package apihttp

import (
	"net/http"
)

type createCreativeWorkOCRReq struct {
	Agent         string `json:"agent"`
	RequestID     string `json:"request_id"`
	SourceAssetID string `json:"source_asset_id"`
}

func (h *handler) createCreativeWorkOCRJob(w http.ResponseWriter, r *http.Request) {
	var req createCreativeWorkOCRReq
	if !decode(w, r, &req) {
		return
	}
	job, err := h.rt.Deps.CreateCreativeWorkOCR(r.Context(), req.Agent, req.SourceAssetID, req.RequestID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *handler) getCreativeWorkOCRJob(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	job, err := h.rt.Deps.GetCreativeWorkOCR(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type creativeWorkOCRAgentReq struct {
	Agent string `json:"agent"`
}

func (h *handler) retryCreativeWorkOCRJob(w http.ResponseWriter, r *http.Request) {
	var req creativeWorkOCRAgentReq
	if !decode(w, r, &req) {
		return
	}
	job, err := h.rt.Deps.RetryCreativeWorkOCR(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type confirmCreativeWorkOCRReq struct {
	Agent           string `json:"agent"`
	ContentMarkdown string `json:"content_markdown"`
}

func (h *handler) confirmCreativeWorkOCRJob(w http.ResponseWriter, r *http.Request) {
	var req confirmCreativeWorkOCRReq
	if !decode(w, r, &req) {
		return
	}
	job, err := h.rt.Deps.ConfirmCreativeWorkOCR(r.Context(), req.Agent, r.PathValue("id"), req.ContentMarkdown)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}
