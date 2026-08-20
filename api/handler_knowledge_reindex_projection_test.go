package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	_ "modernc.org/sqlite"
)

type reindexVectorProjectionHTTPStub struct {
	semanticIndexServiceStub
	projections map[string]knowledge.DocumentVectorProjection
	calls       int
	ownerID     string
	corpusID    string
}

func newReindexVectorProjectionHTTPStub() *reindexVectorProjectionHTTPStub {
	stub := &reindexVectorProjectionHTTPStub{}
	stub.getPolicyFn = func(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error) {
		return knowledge.EmbeddingPolicyProjection{}, nil
	}
	stub.applyFn = func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
		return knowledge.ApplyPolicyResult{}, nil
	}
	stub.getJobFn = func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
		return knowledge.KnowledgeJob{}, nil
	}
	stub.cancelJobFn = stub.getJobFn
	return stub
}

func (s *reindexVectorProjectionHTTPStub) ListDocumentVectorProjections(
	_ context.Context,
	ownerID, corpusID string,
) (map[string]knowledge.DocumentVectorProjection, error) {
	s.calls++
	s.ownerID, s.corpusID = ownerID, corpusID
	return s.projections, nil
}

func TestReindexDocumentReturnsExistingVectorChildJobAsAccepted(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "reindex-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	manager := knowledge.NewManager(store, store, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400),
			splitter.WithRecursiveChunkOverlap(40),
		)),
	)
	doc, err := manager.AddDocument(ctx, "reindex.txt", "可轮询的语义重建正文", "manual")
	if err != nil {
		t.Fatal(err)
	}

	stub := newReindexVectorProjectionHTTPStub()
	stub.projections = map[string]knowledge.DocumentVectorProjection{
		doc.ID: {
			DocumentID:       doc.ID,
			VectorIndexState: knowledge.VectorIndexPending,
			JobID:            "job-reindex-child",
			JobState:         knowledge.KnowledgeJobQueued,
			Stage:            knowledge.JobStageEmbedding,
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(manager)
	srv.SetSemanticIndexService(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/documents/"+doc.ID+"/reindex", nil)
	req.SetPathValue("id", doc.ID)
	rec := httptest.NewRecorder()

	srv.handleReindexDocument(rec, req)

	var payload struct {
		ID               string                      `json:"id"`
		JobID            string                      `json:"job_id"`
		JobState         knowledge.KnowledgeJobState `json:"job_state"`
		VectorIndexState knowledge.VectorIndexState  `json:"vector_index_state"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusAccepted || payload.ID != doc.ID ||
		payload.JobID != "job-reindex-child" || payload.JobState != knowledge.KnowledgeJobQueued ||
		payload.VectorIndexState != knowledge.VectorIndexPending {
		t.Fatalf("status=%d payload=%+v", rec.Code, payload)
	}
	if stub.calls != 1 || stub.ownerID != knowledgeDesktopPrincipalID || stub.corpusID != knowledgeDefaultCorpusID {
		t.Fatalf("projection scope/calls=%q/%q/%d", stub.ownerID, stub.corpusID, stub.calls)
	}
}
