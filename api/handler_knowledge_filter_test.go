package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"

	_ "modernc.org/sqlite"
)

// /api/v1/knowledge/search 端点应把 source_types / sources / 日期过滤透传到检索层，
// 在 topK 截断前生效。这里直接灌库（绕开 splitter），构造 Server 调处理器验证端到端。
func TestHandleSearchKnowledge_MetadataFilter(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	add := func(id, source, stype string, created time.Time) {
		doc := &knowledge.Document{ID: id, Title: id, Source: source, SourceType: stype, ChunkCount: 1, CreatedAt: created, UpdatedAt: created, Status: "indexed"}
		ch := &knowledge.Chunk{ID: id + "-c0", DocID: id, DocTitle: id, Source: source, ChunkCount: 1, Content: "shared widget content", Index: 0, CreatedAt: created}
		if id == "A" {
			ch.PageStart, ch.PageEnd = 7, 7
			ch.SourceDigest = strings.Repeat("a", 64)
			ch.SourceOffsetStart, ch.SourceOffsetEnd = 300, 360
		}
		if err := store.Add(ctx, doc, []*knowledge.Chunk{ch}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	jun01 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	jun20 := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	add("A", "upload:a.pdf", "upload", jun01)
	add("B", "agent", "agent", jun20)

	mgr := knowledge.NewManager(store, store, nil) // nil embedder → 纯关键词读路径
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)

	post := func(body string) []knowledge.SearchHit {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleSearchKnowledge(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Results []knowledge.SearchHit `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, w.Body.String())
		}
		return resp.Results
	}
	docIDs := func(hits []knowledge.SearchHit) map[string]bool {
		m := map[string]bool{}
		for _, h := range hits {
			m[h.DocID] = true
		}
		return m
	}

	// 无过滤：A,B 都召回。
	if got := docIDs(post(`{"query":"widget","top_k":10}`)); !got["A"] || !got["B"] {
		t.Fatalf("无过滤应含 A,B，得 %v", got)
	}
	// source_types=agent：只剩 B，且 Metadata 回填 source_type。
	bOnly := post(`{"query":"widget","top_k":10,"source_types":["agent"]}`)
	if g := docIDs(bOnly); !g["B"] || g["A"] {
		t.Fatalf("source_types=agent 应只剩 B，得 %v", g)
	}
	if st, _ := bOnly[0].Metadata["source_type"].(string); st != "agent" {
		t.Errorf("Metadata.source_type 应回填 agent，得 %v", bOnly[0].Metadata)
	}
	aOnly := post(`{"query":"widget","top_k":10,"sources":["upload:a.pdf"]}`)
	if len(aOnly) != 1 || aOnly[0].PageStart != 7 || aOnly[0].PageEnd != 7 ||
		aOnly[0].SourceDigest != strings.Repeat("a", 64) ||
		aOnly[0].SourceOffsetStart != 300 || aOnly[0].SourceOffsetEnd != 360 {
		t.Fatalf("search API structured source span=%+v", aOnly)
	}
	// created_before=06-10：只剩 A（日期在 Go 层按真实时刻过滤）。
	if g := docIDs(post(`{"query":"widget","top_k":10,"created_before":"2026-06-10"}`)); !g["A"] || g["B"] {
		t.Fatalf("created_before=06-10 应只剩 A，得 %v", g)
	}
	// 非法日期 → 400。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(`{"query":"widget","created_after":"nope"}`))
	w := httptest.NewRecorder()
	srv.handleSearchKnowledge(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法日期应 400，得 %d", w.Code)
	}
}
