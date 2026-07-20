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

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestKnowledgeHTTPQueryUserIDCannotEscapeDesktopDefaultScope(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "knowledge-http-scope.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	base := knowledge.NewSQLiteStore(db)
	if err := base.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeDocumentScopeV27,
	}); err != nil {
		t.Fatal(err)
	}
	repository := knowledge.NewSQLiteSemanticIndexRepository(db)
	for _, owner := range []string{"desktop-user", "other-owner"} {
		if _, err := repository.BindLegacyDefaultCorpus(ctx, owner, "default"); err != nil {
			t.Fatal(err)
		}
	}
	newManager := func(owner string) *knowledge.Manager {
		store := knowledge.NewSQLiteStore(db, knowledge.WithSQLiteSemanticMutations(owner, "default"))
		return knowledge.NewManager(store, store, nil,
			knowledge.WithSplitter(splitter.NewRecursiveSplitter(
				splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(40))))
	}
	desktopManager := newManager("desktop-user")
	otherManager := newManager("other-owner")
	desktopDoc, err := desktopManager.AddDocument(ctx, "lesson.txt", "desktop_scope_token", "upload:lesson.txt")
	if err != nil {
		t.Fatal(err)
	}
	otherDoc, err := otherManager.AddDocument(ctx, "lesson.txt", "other_scope_token", "upload:lesson.txt")
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(desktopManager)
	handler := srv.routes()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	list := request(http.MethodGet, "/api/v1/knowledge/documents?user_id=other-owner", "")
	var listPayload struct {
		Documents []knowledge.Document `json:"documents"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listPayload); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(listPayload.Documents) != 1 || listPayload.Documents[0].ID != desktopDoc.ID {
		t.Fatalf("forged list status=%d payload=%+v", list.Code, listPayload)
	}

	if detail := request(http.MethodGet,
		"/api/v1/knowledge/documents/"+otherDoc.ID+"?user_id=other-owner", ""); detail.Code != http.StatusNotFound {
		t.Fatalf("forged detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	search := request(http.MethodPost,
		"/api/v1/knowledge/search?user_id=other-owner", `{"query":"other_scope_token","top_k":10}`)
	var searchPayload struct {
		Results []knowledge.SearchResult `json:"results"`
		Total   int                      `json:"total"`
	}
	if err := json.NewDecoder(search.Body).Decode(&searchPayload); err != nil {
		t.Fatal(err)
	}
	if search.Code != http.StatusOK || searchPayload.Total != 0 || len(searchPayload.Results) != 0 {
		t.Fatalf("forged search status=%d payload=%+v", search.Code, searchPayload)
	}

	for _, endpoint := range []struct {
		method, suffix string
	}{
		{http.MethodDelete, ""},
		{http.MethodPost, "/reindex"},
	} {
		response := request(endpoint.method,
			"/api/v1/knowledge/documents/"+otherDoc.ID+endpoint.suffix+"?user_id=other-owner", "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("forged %s status=%d body=%s", endpoint.method, response.Code, response.Body.String())
		}
	}
	if got, err := otherManager.GetDocument(ctx, otherDoc.ID); err != nil || got.ID != otherDoc.ID {
		t.Fatalf("forged commands mutated other corpus: got=%+v err=%v", got, err)
	}
}
