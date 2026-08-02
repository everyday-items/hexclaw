package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// ════════════════════════════════════════════════
// Round 4: 残余边界 + 前四轮遗漏
// ════════════════════════════════════════════════

// ── 1. Add: Fields map 共享引用（调用方修改影响已存储条目） ──

func TestLogCollector_Add_FieldsSharedReference(t *testing.T) {
	c := NewLogCollector(10)

	fields := map[string]any{"key": "original"}
	c.Add("info", "test", "msg", fields)

	// 调用方修改 fields — 如果 LogEntry 持有同一 map 引用，已存储的条目也会被改
	fields["key"] = "mutated"
	fields["extra"] = "injected"

	entries, _ := c.Query("", "", "", 1, 0)
	if len(entries) == 0 {
		t.Fatal("no entries")
	}

	if entries[0].Fields["key"] == "mutated" {
		t.Error("LogEntry.Fields still shares reference with caller's map after fix")
	}
	if entries[0].Fields["key"] != "original" {
		t.Errorf("Fields[key] = %v, want 'original'", entries[0].Fields["key"])
	}
	if _, exists := entries[0].Fields["extra"]; exists {
		t.Error("injected field should not appear in stored entry")
	}
}

// ── 2. Add: source="" 的条目被覆盖时，统计异常 ──

func TestLogCollector_Add_EmptySourceOverwrite(t *testing.T) {
	// source="" 的条目写入时不 ++ bySource
	// 但 ring buffer 满后覆盖 source="" 的条目时，
	// old.Source="" → 跳过减操作（if old.Source != ""）
	// 这是正确的 — 验证一致性
	c := NewLogCollector(3)

	c.Add("info", "", "m1", nil) // source="" → bySource 不变
	c.Add("info", "a", "m2", nil)
	c.Add("info", "a", "m3", nil)

	// 满了，覆盖 m1 (source="")
	c.Add("info", "b", "m4", nil)

	stats := c.Stats()
	if stats.BySource["a"] != 2 {
		t.Errorf("source a = %d, want 2", stats.BySource["a"])
	}
	if stats.BySource["b"] != 1 {
		t.Errorf("source b = %d, want 1", stats.BySource["b"])
	}
	if _, exists := stats.BySource[""]; exists {
		t.Errorf("empty source should not appear in stats")
	}
}

// ── 3. Query: limit=0 边界 ──

func TestLogCollector_Query_LimitZero(t *testing.T) {
	c := NewLogCollector(10)
	c.Add("info", "test", "msg", nil)

	// limit=0 时不应返回任何条目，但 total 应正确
	entries, total := c.Query("", "", "", 0, 0)
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %d, want 0 for limit=0", len(entries))
	}
}

func TestLogCollector_Query_LimitNegative(t *testing.T) {
	c := NewLogCollector(10)
	c.Add("info", "test", "msg", nil)

	// 修复后：负数 limit 被钳制为 0，不 panic
	entries, total := c.Query("", "", "", -1, 0)
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if entries != nil {
		t.Errorf("entries should be nil for limit<=0, got %d", len(entries))
	}
}

func TestLogCollector_Query_OffsetNegative(t *testing.T) {
	c := NewLogCollector(10)
	for range 5 {
		c.Add("info", "test", "msg", nil)
	}

	// offset<0 — 在快速路径: offset >= total (true if offset < 0? no, -1 < 5)
	// n = min(limit, total - offset) = min(10, 5-(-1)) = min(10, 6) = 6
	// 但 ring buffer 只有 5 条 → idx 越界？
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Query with negative offset panicked: %v", r)
			t.Log("BUG: Query does not validate offset, negative value may cause out-of-bounds")
		}
	}()

	entries, total := c.Query("", "", "", 10, -1)
	// 如果没 panic，检查结果合理性
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	_ = entries
}

// ── 4. 全部 API v1 路径统一 deny-by-default ──

func TestApiAuth_AllAPIV1PathsRequireAuthentication(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := s.apiAuthMiddleware(inner)

	for _, path := range []string{
		"/api/v1/login", "/api/v1/logout", "/api/v1/logstash",
		"/api/v1/logs", "/api/v1/logs/stats", "/api/v1/logs/stream",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "evil.com:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("path %q should be protected by the API-wide guard, got %d", path, w.Code)
		}
	}
}

// ── 5. NewLogCollector(1) — 最小容量边界 ──

func TestLogCollector_MinCapacity(t *testing.T) {
	c := NewLogCollector(1)

	c.Add("info", "a", "m1", nil)

	stats := c.Stats()
	if stats.Total != 1 {
		t.Fatalf("total = %d, want 1", stats.Total)
	}

	// 覆盖唯一的槽位
	c.Add("warn", "b", "m2", nil)

	stats = c.Stats()
	if stats.Total != 1 {
		t.Fatalf("total = %d, want 1", stats.Total)
	}
	if stats.ByLevel["info"] != 0 {
		t.Errorf("info = %d, want 0 (overwritten)", stats.ByLevel["info"])
	}
	if stats.ByLevel["warn"] != 1 {
		t.Errorf("warn = %d, want 1", stats.ByLevel["warn"])
	}

	entries, total := c.Query("", "", "", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("entries=%d total=%d", len(entries), total)
	}
	if entries[0].Message != "m2" {
		t.Errorf("entry = %q, want m2", entries[0].Message)
	}
}

// ── 6. Stats map 复制：返回值修改不影响内部状态 ──

func TestLogCollector_Stats_ReturnedMapIsolated(t *testing.T) {
	c := NewLogCollector(10)
	c.Add("info", "test", "msg", nil)

	stats := c.Stats()
	// 修改返回的 map
	stats.ByLevel["info"] = 999
	stats.BySource["test"] = 999

	// 内部状态不应受影响
	stats2 := c.Stats()
	if stats2.ByLevel["info"] != 1 {
		t.Errorf("internal ByLevel mutated: info=%d, want 1", stats2.ByLevel["info"])
	}
}

// ── 7. handleGetLogs: offset 无上限 ──

func TestHandleGetLogs_HugeOffset(t *testing.T) {
	s := &Server{logCollector: NewLogCollector(10)}
	s.logCollector.Add("info", "test", "msg", nil)

	// offset=999999999 不应导致问题
	req := httptest.NewRequest("GET", "/api/v1/logs?offset=999999999", nil)
	w := httptest.NewRecorder()
	s.handleGetLogs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// ── 8. 基准: Query 在 capacity=1 的极端条件 ──

func BenchmarkLogCollector_Query_Cap1(b *testing.B) {
	c := NewLogCollector(1)
	c.Add("info", "test", "msg", nil)
	b.ResetTimer()
	for range b.N {
		c.Query("", "", "", 10, 0)
	}
}

// ── 9. 快速路径 offset > 0 的正确性 ──

func TestLogCollector_Query_Unfiltered_WithOffset(t *testing.T) {
	c := NewLogCollector(100)
	for i := range 20 {
		c.Add("info", "test", fmt.Sprintf("msg-%02d", i), nil)
	}

	// offset=5, limit=3 → 应跳过最新 5 条，返回第 6-8 新的
	entries, total := c.Query("", "", "", 3, 5)
	if total != 20 {
		t.Fatalf("total = %d, want 20", total)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// 最新是 msg-19 (offset=0), msg-18 (1), ..., msg-14 (5), msg-13 (6), msg-12 (7)
	if !strings.Contains(entries[0].Message, "msg-14") {
		t.Errorf("first = %q, want msg-14", entries[0].Message)
	}
	if !strings.Contains(entries[2].Message, "msg-12") {
		t.Errorf("last = %q, want msg-12", entries[2].Message)
	}
}

// ── 10. 快速路径 offset+limit 超过 size ──

func TestLogCollector_Query_Unfiltered_OffsetPlusLimitExceedsSize(t *testing.T) {
	c := NewLogCollector(100)
	for i := range 5 {
		c.Add("info", "test", fmt.Sprintf("msg-%d", i), nil)
	}

	// offset=3, limit=100 → 只有 2 条可返回
	entries, total := c.Query("", "", "", 100, 3)
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2", len(entries))
	}
}

// ── 11. ring buffer 环绕后的 Query 正确性 ──

func TestLogCollector_Query_AfterWrap(t *testing.T) {
	c := NewLogCollector(5)

	// 写入 8 条（环绕）: 保留 msg-3..msg-7
	for i := range 8 {
		c.Add("info", "test", fmt.Sprintf("msg-%d", i), nil)
	}

	entries, total := c.Query("", "", "", 100, 0)
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(entries))
	}
	// 最新 → 最旧: msg-7, msg-6, msg-5, msg-4, msg-3
	if !strings.Contains(entries[0].Message, "msg-7") {
		t.Errorf("newest = %q, want msg-7", entries[0].Message)
	}
	if !strings.Contains(entries[4].Message, "msg-3") {
		t.Errorf("oldest = %q, want msg-3", entries[4].Message)
	}
}

// ── 12. isLogsAPI 不应影响 POST /api/v1/logs (如果存在) ──

func TestApiAuth_LogsStreamAlsoProtected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := s.apiAuthMiddleware(inner)

	// /api/v1/logs/stream 也应被保护
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil)
	req.RemoteAddr = "evil.com:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/logs/stream: status=%d, want 401", w.Code)
	}

	// /api/v1/logs/stats 也应被保护
	req2 := httptest.NewRequest("GET", "/api/v1/logs/stats", nil)
	req2.RemoteAddr = "evil.com:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/logs/stats: status=%d, want 401", w2.Code)
	}
}
