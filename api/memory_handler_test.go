package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
