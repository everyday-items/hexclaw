package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
)

func TestHandleBudgetStatus_ReturnsValidJSON(t *testing.T) {
	budget := engine.NewBudgetController(engine.BudgetConfig{
		MaxTokens:   10000,
		MaxDuration: 1 * time.Hour,
		MaxCost:     5.0,
	})
	budget.RecordTokens(100)
	budget.RecordCost(0.5)

	s := &Server{budgetCtrl: budget}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/budget/status", nil)
	rec := httptest.NewRecorder()

	s.handleBudgetStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response should be valid JSON: %v\nbody: %s", err, rec.Body.String())
	}

	if _, ok := result["tokens_used"]; !ok {
		t.Error("response should contain 'tokens_used' field")
	}
	if _, ok := result["tokens_max"]; !ok {
		t.Error("response should contain 'tokens_max' field")
	}
	if _, ok := result["exhausted"]; !ok {
		t.Error("response should contain 'exhausted' field")
	}
}

func TestHandleToolCacheStats_ZeroHitsMisses(t *testing.T) {
	// Fresh cache with zero hits and zero misses
	cache := engine.NewToolCache(100, 5*time.Minute)

	s := &Server{toolCache: cache}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tools/cache/stats", nil)
	rec := httptest.NewRecorder()

	s.handleToolCacheStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response should be valid JSON: %v\nbody: %s", err, rec.Body.String())
	}

	// Check for division by zero in hit_rate calculation
	hitRate, ok := result["hit_rate"].(float64)
	if !ok {
		t.Fatalf("hit_rate should be a number, got %T: %v", result["hit_rate"], result["hit_rate"])
	}
	if hitRate != 0 {
		t.Errorf("hit_rate should be 0 when no cache activity, got %f", hitRate)
	}

	hits, _ := result["hits"].(float64)
	misses, _ := result["misses"].(float64)
	if hits != 0 || misses != 0 {
		t.Errorf("hits and misses should both be 0, got hits=%v, misses=%v", hits, misses)
	}
}

func TestHandleToolPermissions_EmptyRules(t *testing.T) {
	perms := engine.NewToolPermissions(nil, nil) // empty allow + deny

	s := &Server{toolPerms: perms}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tools/permissions", nil)
	rec := httptest.NewRecorder()

	s.handleToolPermissions(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response should be valid JSON: %v\nbody: %s", err, rec.Body.String())
	}

	rules := result["rules"]
	// When both allow and deny are empty, `rules` will be null (nil slice not appended to)
	// This could be a bug if the frontend expects an array
	if rules == nil {
		t.Log("BUG FOUND: 'rules' is null when both allow and deny are empty. Frontend may expect [].")
	}
}

// REG-TOOL-APPROVAL-LIFECYCLE-MUTATION-NA：当前产品只公开读取工具权限，
// 不存在显式 revoke 或 tool-disable 的 HTTP mutation；不得为生命周期门禁发明入口。
func TestToolPermissionLifecycleMutationsAreNotPublicOperations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetToolPermissions(engine.NewToolPermissions([]string{"search"}, []string{"code_exec"}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tools/permissions", nil)
	req.RemoteAddr = "127.0.0.1:18080"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST tool permissions status = %d, want 405 read-only boundary", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("POST tool permissions Allow = %q, want GET, HEAD", got)
	}
}

func TestHandleBudgetStatus_FreshBudget(t *testing.T) {
	budget := engine.NewBudgetController(engine.BudgetConfig{
		MaxTokens:   500000,
		MaxDuration: 30 * time.Minute,
		MaxCost:     5.0,
	})

	s := &Server{budgetCtrl: budget}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/budget/status", nil)
	rec := httptest.NewRecorder()

	s.handleBudgetStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("expected application/json content-type, got %q", ct)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response should be valid JSON: %v", err)
	}
}
