package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type pendingVectorHTTPResolver struct{}

func (pendingVectorHTTPResolver) Resolve(
	context.Context,
	string,
	string,
	knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	return knowledge.EmbeddingProfileSnapshot{
		Profile: knowledge.EmbeddingProfile{
			ProfileID: "local-test", ModelName: "test-embedding", ProviderID: "local",
			ProviderName: "local", Location: knowledge.ProviderLocationLocal,
			Capability: "embedding", Dimension: 3, Availability: knowledge.ProfileAvailabilityInstalled,
		},
		Normalization: "l2", ChunkConfigHash: "chunk-v1", ProfileConfigHash: "profile-v1",
	}, nil
}

func (pendingVectorHTTPResolver) Catalog(
	context.Context,
	string,
	string,
) (knowledge.EmbeddingProfileCatalog, error) {
	return knowledge.EmbeddingProfileCatalog{}, nil
}

func TestKnowledgeAcceptedUploadImmediateListKeepsVectorPending(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pending-vector-http.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	store := knowledge.NewSQLiteStore(db, knowledge.WithSQLiteSemanticMutations("desktop-user", "default"))
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeIngestV24,
		migrate.KnowledgeIngestGenerationsV26,
		migrate.KnowledgeDocumentScopeV27,
		migrate.KnowledgeIngestCheckpointV28,
		migrate.KnowledgeUploadOperationsV71,
	}); err != nil {
		t.Fatal(err)
	}
	repository := knowledge.NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "default"); err != nil {
		t.Fatal(err)
	}
	semantic := knowledge.NewSemanticIndexService(repository, pendingVectorHTTPResolver{})
	if _, err := semantic.EnsureDefaultPolicy(ctx, "desktop-user", "default"); err != nil {
		t.Fatal(err)
	}
	if err := semantic.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(knowledge.NewManager(store, store, nil))
	srv.SetSemanticIndexService(semantic)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	part, err := writer.CreateFormFile("file", "accepted.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("accepted asynchronous source")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/knowledge/documents", &requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Idempotency-Key", "accepted-immediate-list")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var accepted knowledge.CreateDocumentResult
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted || accepted.DocumentID == "" ||
		accepted.TextIndexState != knowledge.TextIndexPending ||
		accepted.VectorIndexState != knowledge.VectorIndexPending {
		t.Fatalf("accepted status=%d payload=%+v", response.StatusCode, accepted)
	}

	listResponse, err := http.Get(ts.URL + "/api/v1/knowledge/documents")
	if err != nil {
		t.Fatal(err)
	}
	defer listResponse.Body.Close()
	var listed struct {
		Documents []knowledge.Document `json:"documents"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if listResponse.StatusCode != http.StatusOK || len(listed.Documents) != 1 ||
		listed.Documents[0].ID != accepted.DocumentID || listed.Documents[0].Status != "processing" ||
		listed.Documents[0].VectorIndexState != knowledge.VectorIndexPending ||
		listed.Documents[0].VectorJobID != "" || listed.Documents[0].VectorError != "" {
		t.Fatalf("immediate list status=%d payload=%+v", listResponse.StatusCode, listed)
	}
}
