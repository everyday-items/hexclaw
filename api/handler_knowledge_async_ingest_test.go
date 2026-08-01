package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	_ "modernc.org/sqlite"
)

type documentIngestHTTPStub struct {
	semanticIndexServiceStub
	t                 *testing.T
	consumedBytes     int64
	createCallCount   int
	ownerID           string
	corpusID          string
	projection        *knowledge.KnowledgeDocumentProjection
	vectorProjections map[string]knowledge.DocumentVectorProjection
}

func (s *documentIngestHTTPStub) ListDocumentVectorProjections(
	_ context.Context,
	ownerID, corpusID string,
) (map[string]knowledge.DocumentVectorProjection, error) {
	if ownerID != "desktop-user" || corpusID != "default" {
		return nil, knowledge.ErrSemanticIndexNotFound
	}
	return s.vectorProjections, nil
}

func (s *documentIngestHTTPStub) GetIngestDocumentProjection(
	_ context.Context,
	ownerID, documentID string,
) (knowledge.KnowledgeDocumentProjection, error) {
	if s.projection == nil || s.projection.OwnerID != ownerID || s.projection.DocumentID != documentID {
		return knowledge.KnowledgeDocumentProjection{}, knowledge.ErrSemanticIndexNotFound
	}
	return *s.projection, nil
}

func (s *documentIngestHTTPStub) GetIngestDocumentProjectionForCorpus(
	ctx context.Context,
	ownerID, corpusID, documentID string,
) (knowledge.KnowledgeDocumentProjection, error) {
	if corpusID != "default" {
		return knowledge.KnowledgeDocumentProjection{}, knowledge.ErrSemanticIndexNotFound
	}
	return s.GetIngestDocumentProjection(ctx, ownerID, documentID)
}

func (s *documentIngestHTTPStub) CreateDocument(
	_ context.Context,
	ownerID, corpusID string,
	input knowledge.CreateDocumentInput,
) (knowledge.CreateDocumentResult, error) {
	s.createCallCount++
	s.ownerID, s.corpusID = ownerID, corpusID
	if input.IdempotencyKey != "six-upper-fixture" || input.Filename != "六上数学.pdf" ||
		input.MediaType != "application/pdf" || input.Subject != "数学" || input.Grade != "六年级上" {
		s.t.Fatalf("create input=%+v", input)
	}
	consumed, err := io.Copy(io.Discard, input.Body)
	if err != nil {
		return knowledge.CreateDocumentResult{}, err
	}
	s.consumedBytes = consumed
	return knowledge.CreateDocumentResult{
		DocumentID: "doc-async", JobID: "job-async",
		TextIndexState: knowledge.TextIndexPending, VectorIndexState: knowledge.VectorIndexPending,
	}, nil
}

func TestKnowledgeDocumentsMultipartAcceptsFrozen57313616BytesAs202AndRetiresOldRoute(t *testing.T) {
	stub := &documentIngestHTTPStub{t: t}
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

	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(&knowledge.Manager{})
	srv.SetSemanticIndexService(stub)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	const size = int64(57_313_616)
	var fileBody io.Reader = io.MultiReader(strings.NewReader("%PDF-1.6\n"),
		io.LimitReader(zeroHTTPReader{}, size-int64(len("%PDF-1.6\n"))))
	if fixturePath := strings.TrimSpace(os.Getenv("HEXCLAW_KNOWLEDGE_REAL_PDF")); fixturePath != "" {
		fixture, err := os.Open(fixturePath)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.Close()
		info, err := fixture.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != size {
			t.Fatalf("real PDF bytes=%d want=%d", info.Size(), size)
		}
		fileBody = fixture
	}
	requestBody, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		for key, value := range map[string]string{
			"corpus_id": "default", "subject": "数学", "grade": "六年级上",
		} {
			if err := multipartWriter.WriteField(key, value); err != nil {
				writeErr <- err
				_ = writer.CloseWithError(err)
				return
			}
		}
		part, err := multipartWriter.CreateFormFile("file", "六上数学.pdf")
		if err == nil {
			_, err = io.Copy(part, fileBody)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
		writeErr <- err
	}()
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/knowledge/documents?user_id=forged-remote-user", requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("Idempotency-Key", "six-upper-fixture")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	var payload knowledge.CreateDocumentResult
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted || payload.DocumentID != "doc-async" ||
		payload.JobID != "job-async" || stub.consumedBytes != size || stub.createCallCount != 1 ||
		stub.ownerID != "desktop-user" || stub.corpusID != "default" {
		t.Fatalf("status=%d payload=%+v bytes=%d calls=%d", resp.StatusCode, payload,
			stub.consumedBytes, stub.createCallCount)
	}

	oldResp, err := http.Post(ts.URL+"/api/v1/knowledge/upload?user_id=desktop-user",
		"multipart/form-data", strings.NewReader("retired"))
	if err != nil {
		t.Fatal(err)
	}
	defer oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusNotFound {
		t.Fatalf("retired upload route status=%d", oldResp.StatusCode)
	}
}

type zeroHTTPReader struct{}

func (zeroHTTPReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestKnowledgeDocumentsRejectsUnsupportedCorpusBeforeQueueing(t *testing.T) {
	stub := &documentIngestHTTPStub{t: t}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("corpus_id", "another-child"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "lesson.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "lesson"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/knowledge/documents?user_id=forged-remote-user", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Idempotency-Key", "unsupported-corpus")
	rec := httptest.NewRecorder()
	srv.handleCreateKnowledgeDocument(rec, req)

	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity || payload["code"] != "knowledge_scope_unsupported" {
		t.Fatalf("status=%d payload=%v", rec.Code, payload)
	}
	if stub.createCallCount != 0 {
		t.Fatalf("unsupported corpus queued %d job(s)", stub.createCallCount)
	}
}

func TestKnowledgeDocumentDetailPrefersAsyncProjectionAndKeepsLegacyContentCompatibility(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "knowledge-detail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	doc := &knowledge.Document{
		ID: "doc-async-detail", Title: "六上数学.pdf", Content: "legacy-compatible extracted text",
		Source: "upload:六上数学.pdf", SourceType: "upload", Status: "indexed", ChunkCount: 1,
	}
	if err := store.Add(ctx, doc, []*knowledge.Chunk{{
		ID: "doc-async-detail-chunk", DocID: doc.ID, DocTitle: doc.Title, Source: doc.Source,
		SourceType: "upload", ChunkCount: 1, Content: doc.Content,
	}}); err != nil {
		t.Fatal(err)
	}
	pageCount := int64(128)
	stub := &documentIngestHTTPStub{
		t: t,
		vectorProjections: map[string]knowledge.DocumentVectorProjection{
			"doc-async-detail": {
				DocumentID: "doc-async-detail", VectorIndexState: knowledge.VectorIndexFailed,
				JobID: "job-vector-failed", JobState: knowledge.KnowledgeJobFailed,
				Stage: knowledge.JobStageEmbedding, LastError: "provider unavailable",
			},
		},
		projection: &knowledge.KnowledgeDocumentProjection{
			DocumentID: "doc-async-detail", DocumentGeneration: 3,
			OwnerID: "desktop-user", CorpusID: "default",
			Filename: "六上数学.pdf", MediaType: "application/pdf", SizeBytes: 57_313_616,
			SHA256: strings.Repeat("a", 64), AgentID: "tutor-a", LearnerID: "learner-a",
			Subject: "数学", Grade: "六年级上", PageCount: &pageCount,
			TextIndexState: knowledge.TextIndexReady, Warnings: []string{},
			SourceSpans: []knowledge.SourceSpan{{
				PageStart: 12, PageEnd: 12, SourceDigest: strings.Repeat("a", 64),
				SourceOffsetStart: 240, SourceOffsetEnd: 360,
			}},
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(knowledge.NewManager(store, store, nil))
	srv.SetSemanticIndexService(stub)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/knowledge/documents/doc-async-detail?user_id=forged-remote-user")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d payload=%v", resp.StatusCode, payload)
	}
	for key, want := range map[string]any{
		"id": "doc-async-detail", "document_id": "doc-async-detail",
		"document_generation": float64(3),
		"content":             "legacy-compatible extracted text", "status": "indexed",
		"source_digest": strings.Repeat("a", 64), "sha256": strings.Repeat("a", 64),
		"owner_id": "desktop-user", "corpus_id": "default", "agent_id": "tutor-a",
		"learner_id": "learner-a", "subject": "数学", "grade": "六年级上",
	} {
		if payload[key] != want {
			t.Errorf("%s=%v, want %v", key, payload[key], want)
		}
	}
	if payload["page_count"] != float64(128) || payload["pages_total"] != float64(128) ||
		payload["pages_done"] != float64(128) || payload["text_index_state"] != "ready" {
		t.Fatalf("async counters/state missing: %v", payload)
	}
	if payload["vector_index_state"] != "failed" || payload["vector_job_id"] != "job-vector-failed" ||
		payload["vector_job_state"] != "failed" || payload["vector_error"] != "provider unavailable" {
		t.Fatalf("vector failure projection missing from detail: %v", payload)
	}
	spans, ok := payload["source_spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("source_spans=%v", payload["source_spans"])
	}

	listResp, err := http.Get(ts.URL + "/api/v1/knowledge/documents")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var listPayload struct {
		Documents []knowledge.Document `json:"documents"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listPayload); err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK || len(listPayload.Documents) != 1 ||
		listPayload.Documents[0].VectorIndexState != knowledge.VectorIndexFailed ||
		listPayload.Documents[0].VectorJobID != "job-vector-failed" {
		t.Fatalf("vector failure projection missing from list: status=%d payload=%+v",
			listResp.StatusCode, listPayload)
	}
	span, ok := spans[0].(map[string]any)
	if !ok || span["page_start"] != float64(12) ||
		span["source_digest"] != strings.Repeat("a", 64) ||
		span["source_offset_start"] != float64(240) {
		t.Fatalf("source span payload=%v", spans[0])
	}
}
