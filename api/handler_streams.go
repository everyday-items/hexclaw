package api

import (
	"net/http"

	"github.com/hexagon-codes/hexclaw/streamstate"
)

func (s *Server) handleListActiveStreams(w http.ResponseWriter, r *http.Request) {
	if s.streamStates == nil {
		writeJSON(w, http.StatusOK, map[string]any{"streams": []streamstate.Snapshot{}, "total": 0})
		return
	}
	userID := sessionUserIDFromRequest(r)
	streams := s.streamStates.ListActiveStreams(userID)
	if streams == nil {
		streams = []streamstate.Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"streams": streams,
		"total":   len(streams),
	})
}

func (s *Server) handleGetStreamSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.streamStates == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
		return
	}
	userID := sessionUserIDFromRequest(r)
	requestID := r.PathValue("request_id")
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id 不能为空"})
		return
	}
	snapshot, ok := s.streamStates.GetStreamSnapshot(userID, requestID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
