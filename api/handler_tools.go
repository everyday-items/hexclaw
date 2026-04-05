package api

import (
	"net/http"
)

// handleBudgetStatus returns current budget usage.
//
// GET /api/v1/budget/status
func (s *Server) handleBudgetStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.budgetCtrl.Status())
}

// handleToolCacheStats returns cache hit/miss statistics.
//
// GET /api/v1/tools/cache/stats
func (s *Server) handleToolCacheStats(w http.ResponseWriter, _ *http.Request) {
	hits, misses := s.toolCache.Stats()
	entries := s.toolCache.EntryCount()
	total := hits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(hits) / float64(total) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":  entries,
		"hits":     hits,
		"misses":   misses,
		"hit_rate": hitRate,
	})
}

// handleToolMetrics returns per-tool call statistics.
//
// GET /api/v1/tools/metrics
func (s *Server) handleToolMetrics(w http.ResponseWriter, _ *http.Request) {
	stats, err := s.toolMetrics.ReadStats(20)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"tools": []any{}, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": stats})
}

// handleToolPermissions returns current allow/deny rules.
//
// GET /api/v1/tools/permissions
func (s *Server) handleToolPermissions(w http.ResponseWriter, _ *http.Request) {
	allow, deny := s.toolPerms.Rules()
	type rule struct {
		Pattern string `json:"pattern"`
		Action  string `json:"action"`
	}
	rules := make([]rule, 0)
	for _, p := range allow {
		rules = append(rules, rule{Pattern: p, Action: "allow"})
	}
	for _, p := range deny {
		rules = append(rules, rule{Pattern: p, Action: "deny"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handleListCheckpoints returns checkpoint list for a session.
//
// GET /api/v1/sessions/{id}/checkpoints
func (s *Server) handleListCheckpoints(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	cp, err := s.checkpointMgr.Load(r.Context(), sessionID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"checkpoints": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": []any{cp}})
}
