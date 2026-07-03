package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/memory"
)

func newTestMemoryServer(t *testing.T, maxMemory int) (*Server, *memory.FileMemory) {
	t.Helper()
	fm, err := memory.New(memory.Options{Dir: filepath.Join(t.TempDir(), "memory"), MaxMemory: maxMemory})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetFileMemory(fm)
	return srv, fm
}

func TestHandleGetMemorySupportsCursorAndView(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)
	for _, content := range []string{"记忆 A", "记忆 B", "记忆 C"} {
		if err := fm.SaveMemory(content); err != nil {
			t.Fatalf("save %q: %v", content, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory?view=active&limit=2", nil)
	w := httptest.NewRecorder()
	srv.handleGetMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}
	var firstPage struct {
		Entries    []memory.MemoryEntry `json:"entries"`
		Total      int                  `json:"total"`
		NextCursor string               `json:"next_cursor"`
		HasMore    bool                 `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if len(firstPage.Entries) != 2 || firstPage.Total != 3 || !firstPage.HasMore || firstPage.NextCursor == "" {
		t.Fatalf("first page=%+v, want 2 entries, total 3, next cursor", firstPage)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/memory?view=active&limit=2&cursor="+firstPage.NextCursor, nil)
	w = httptest.NewRecorder()
	srv.handleGetMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}
	var secondPage struct {
		Entries    []memory.MemoryEntry `json:"entries"`
		NextCursor string               `json:"next_cursor"`
		HasMore    bool                 `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &secondPage); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(secondPage.Entries) != 1 || secondPage.HasMore || secondPage.NextCursor != "" {
		t.Fatalf("second page=%+v, want final page", secondPage)
	}
}

func TestHandleArchiveAndRestoreMemoryItem(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)
	if err := fm.SaveMemory("需要归档再恢复的记忆"); err != nil {
		t.Fatalf("save: %v", err)
	}
	active := fm.ParseEntries()
	if len(active) != 1 {
		t.Fatalf("active len=%d, want 1", len(active))
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/"+active[0].ID+"/archive", nil)
	req.SetPathValue("id", active[0].ID)
	w := httptest.NewRecorder()
	srv.handleArchiveMemoryItem(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("archive status=%d, body=%s", w.Code, w.Body.String())
	}

	archived, err := fm.ListEntries(memory.ListOptions{View: memory.MemoryViewArchived, Limit: 10})
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(archived.Entries) != 1 || archived.Entries[0].Status != memory.MemoryStatusArchived {
		t.Fatalf("archived=%+v, want one archived entry", archived.Entries)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/memory/"+archived.Entries[0].ID+"/restore", nil)
	req.SetPathValue("id", archived.Entries[0].ID)
	w = httptest.NewRecorder()
	srv.handleRestoreMemoryItem(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("restore status=%d, body=%s", w.Code, w.Body.String())
	}

	restored := fm.ParseEntries()
	if len(restored) != 1 || restored[0].Content != "需要归档再恢复的记忆" {
		t.Fatalf("restored=%+v, want restored active entry", restored)
	}
}

// --- handleGetMemory 测试 ---

func TestGetMemory_Empty(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory", nil)
	w := httptest.NewRecorder()
	srv.handleGetMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Entries  []memory.MemoryEntry  `json:"entries"`
		Total    int                   `json:"total"`
		HasMore  bool                  `json:"has_more"`
		Capacity memory.MemoryCapacity `json:"capacity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("entries 应为空，实际长度 %d", len(resp.Entries))
	}
	if resp.Total != 0 {
		t.Errorf("total=%d, want 0", resp.Total)
	}
	if resp.HasMore {
		t.Error("has_more 应为 false")
	}
}

func TestGetMemory_WithEntries(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	for _, content := range []string{"用户喜欢 Go 语言", "项目使用 SQLite 数据库"} {
		if err := fm.SaveMemory(content); err != nil {
			t.Fatalf("save %q: %v", content, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory", nil)
	w := httptest.NewRecorder()
	srv.handleGetMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Entries  []memory.MemoryEntry  `json:"entries"`
		Total    int                   `json:"total"`
		Summary  string                `json:"summary"`
		Capacity memory.MemoryCapacity `json:"capacity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total=%d, want 2", resp.Total)
	}
	if len(resp.Entries) != 2 {
		t.Errorf("entries len=%d, want 2", len(resp.Entries))
	}
}

func TestGetMemory_InvalidLimit(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory?limit=abc", nil)
	w := httptest.NewRecorder()
	srv.handleGetMemory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("无效 limit 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- handleSaveMemory 测试 ---

func TestSaveMemory_Success(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	body := `{"content":"用户偏好深色主题","type":"preference","source":"manual"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleSaveMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp memory.MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Content != "用户偏好深色主题" {
		t.Errorf("content=%q, want 用户偏好深色主题", resp.Content)
	}
	if resp.ID == "" {
		t.Error("id 不应为空")
	}

	// 验证持久化
	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("entries len=%d, want 1", len(entries))
	}
	if entries[0].Content != "用户偏好深色主题" {
		t.Errorf("persisted content=%q", entries[0].Content)
	}
}

func TestSaveMemory_ManualAPIDoesNotDropDistinctShortEntries(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)
	if err := fm.SaveStructuredEntry("manual e2e probe", "fact", "manual", "", memory.EntryMeta{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"content":"e2e memory readback marker"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleSaveMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}
	var resp memory.MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Content != "e2e memory readback marker" {
		t.Fatalf("POST response content=%q, want new marker", resp.Content)
	}

	entries := fm.ParseEntries()
	if len(entries) != 2 {
		t.Fatalf("entries len=%d, want 2: %+v", len(entries), entries)
	}
	if entries[1].Content != "e2e memory readback marker" {
		t.Fatalf("persisted second content=%q", entries[1].Content)
	}
}

func TestSaveMemory_EmptyContent(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	body := `{"content":"","type":"fact","source":"manual"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleSaveMemory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 content 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp["error"], "content 不能为空") {
		t.Errorf("error=%q, should contain 'content 不能为空'", resp["error"])
	}
}

func TestSaveMemory_InvalidJSON(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory", strings.NewReader(`{bad json`))
	w := httptest.NewRecorder()
	srv.handleSaveMemory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("无效 JSON 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- handleDeleteMemory (clear all) 测试 ---

func TestDeleteMemory_ClearAll(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	// 先写入一些记忆
	for _, content := range []string{"记忆 A", "记忆 B", "记忆 C"} {
		if err := fm.SaveMemory(content); err != nil {
			t.Fatalf("save %q: %v", content, err)
		}
	}
	if len(fm.ParseEntries()) != 3 {
		t.Fatal("预期 3 条记忆")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memory", nil)
	w := httptest.NewRecorder()
	srv.handleDeleteMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["message"] != "所有记忆已清空" {
		t.Errorf("message=%q, want 所有记忆已清空", resp["message"])
	}

	// 确认所有记忆已清空
	entries := fm.ParseEntries()
	if len(entries) != 0 {
		t.Errorf("清空后仍有 %d 条记忆", len(entries))
	}
}

// --- handleDeleteMemoryItem (single delete) 测试 ---

func TestDeleteMemoryItem_Success(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	if err := fm.SaveMemory("要删除的单条记忆"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := fm.SaveMemory("保留的记忆"); err != nil {
		t.Fatalf("save: %v", err)
	}

	entries := fm.ParseEntries()
	if len(entries) != 2 {
		t.Fatalf("entries len=%d, want 2", len(entries))
	}
	targetID := entries[0].ID

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memory/"+targetID, nil)
	req.SetPathValue("id", targetID)
	w := httptest.NewRecorder()
	srv.handleDeleteMemoryItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["message"] != "记忆已删除" {
		t.Errorf("message=%q, want 记忆已删除", resp["message"])
	}

	// 确认只剩一条
	remaining := fm.ParseEntries()
	if len(remaining) != 1 {
		t.Errorf("删除后 entries len=%d, want 1", len(remaining))
	}
	if remaining[0].Content != "保留的记忆" {
		t.Errorf("remaining content=%q, want 保留的记忆", remaining[0].Content)
	}
}

func TestDeleteMemoryItem_NotFound(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memory/nonexistent-id", nil)
	req.SetPathValue("id", "nonexistent-id")
	w := httptest.NewRecorder()
	srv.handleDeleteMemoryItem(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的记忆应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteMemoryItem_EmptyID(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memory/", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleDeleteMemoryItem(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 ID 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- handleSearchMemory 测试 ---

func TestSearchMemory_EmptyQuery(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/search", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMemory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空查询应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp["error"], "q 参数不能为空") {
		t.Errorf("error=%q, should contain 'q 参数不能为空'", resp["error"])
	}
}

func TestSearchMemory_WithResults(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	for _, content := range []string{
		"用户喜欢 Golang 编程",
		"项目使用 PostgreSQL 数据库",
		"用户偏好 Vim 编辑器",
	} {
		if err := fm.SaveMemory(content); err != nil {
			t.Fatalf("save %q: %v", content, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/search?q=用户", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Results       []memory.SearchResult `json:"results"`
		VectorResults []any                 `json:"vector_results"`
		Total         int                   `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 应至少匹配 "用户喜欢 Golang 编程" 和 "用户偏好 Vim 编辑器"
	if resp.Total < 2 {
		t.Errorf("total=%d, want >= 2", resp.Total)
	}
	if len(resp.Results) < 2 {
		t.Errorf("results len=%d, want >= 2", len(resp.Results))
	}
}

func TestSearchMemory_NoResults(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	if err := fm.SaveMemory("只有这一条记忆"); err != nil {
		t.Fatalf("save: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/search?q=完全不匹配的关键词xyz", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMemory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []any `json:"results"`
		Total   int   `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("total=%d, want 0", resp.Total)
	}
}

// --- handleArchiveMemoryItem 测试 ---

func TestArchiveMemoryItem_Success(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	if err := fm.SaveMemory("待归档的记忆"); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("entries len=%d, want 1", len(entries))
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/"+entries[0].ID+"/archive", nil)
	req.SetPathValue("id", entries[0].ID)
	w := httptest.NewRecorder()
	srv.handleArchiveMemoryItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["message"] != "记忆已归档" {
		t.Errorf("message=%q, want 记忆已归档", resp["message"])
	}

	// 确认 active 列表为空
	activeEntries := fm.ParseEntries()
	if len(activeEntries) != 0 {
		t.Errorf("归档后 active entries len=%d, want 0", len(activeEntries))
	}

	// 确认在 archived 列表中
	archived, err := fm.ListEntries(memory.ListOptions{View: memory.MemoryViewArchived, Limit: 10})
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(archived.Entries) != 1 {
		t.Errorf("archived entries len=%d, want 1", len(archived.Entries))
	}
}

func TestArchiveMemoryItem_NotFound(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/nonexistent-id/archive", nil)
	req.SetPathValue("id", "nonexistent-id")
	w := httptest.NewRecorder()
	srv.handleArchiveMemoryItem(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("归档不存在的记忆应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestArchiveMemoryItem_EmptyID(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory//archive", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleArchiveMemoryItem(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 ID 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- handleRestoreMemoryItem 测试 ---

func TestRestoreMemoryItem_Success(t *testing.T) {
	srv, fm := newTestMemoryServer(t, 10)

	if err := fm.SaveMemory("先归档再恢复"); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("entries len=%d, want 1", len(entries))
	}
	id := entries[0].ID

	// 先归档
	if err := fm.ArchiveEntry(id); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// 归档后 ID 前缀从 "m-" 变为 "a-"，需从归档列表获取新 ID
	archived, err := fm.ListEntries(memory.ListOptions{View: memory.MemoryViewArchived, Limit: 10})
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(archived.Entries) != 1 {
		t.Fatalf("archived entries len=%d, want 1", len(archived.Entries))
	}
	archivedID := archived.Entries[0].ID

	// 通过 API 恢复
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/"+archivedID+"/restore", nil)
	req.SetPathValue("id", archivedID)
	w := httptest.NewRecorder()
	srv.handleRestoreMemoryItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["message"] != "记忆已恢复" {
		t.Errorf("message=%q, want 记忆已恢复", resp["message"])
	}

	// 确认恢复到 active 列表
	activeEntries := fm.ParseEntries()
	if len(activeEntries) != 1 {
		t.Errorf("恢复后 active entries len=%d, want 1", len(activeEntries))
	}
	if activeEntries[0].Content != "先归档再恢复" {
		t.Errorf("content=%q, want 先归档再恢复", activeEntries[0].Content)
	}
}

func TestRestoreMemoryItem_NotFound(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/nonexistent-id/restore", nil)
	req.SetPathValue("id", "nonexistent-id")
	w := httptest.NewRecorder()
	srv.handleRestoreMemoryItem(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("恢复不存在的记忆应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestRestoreMemoryItem_EmptyID(t *testing.T) {
	srv, _ := newTestMemoryServer(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory//restore", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleRestoreMemoryItem(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 ID 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}
