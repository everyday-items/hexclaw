package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

type retryDocumentHTTPStub struct {
	semanticIndexServiceStub
	retryFn func(context.Context, string, string, string, string) (knowledge.CreateDocumentResult, error)
	calls   int
}

func (s *retryDocumentHTTPStub) RetryDocument(
	ctx context.Context,
	ownerID, corpusID, documentID, idempotencyKey string,
) (knowledge.CreateDocumentResult, error) {
	s.calls++
	return s.retryFn(ctx, ownerID, corpusID, documentID, idempotencyKey)
}

func newRetryDocumentHTTPStub(t *testing.T) *retryDocumentHTTPStub {
	t.Helper()
	stub := &retryDocumentHTTPStub{}
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

func TestKnowledgeDocumentRetryRequiresIdempotencyKeyAndUsesTrustedScope(t *testing.T) {
	stub := newRetryDocumentHTTPStub(t)
	stub.retryFn = func(_ context.Context, ownerID, corpusID, documentID, key string) (knowledge.CreateDocumentResult, error) {
		if ownerID != "desktop-user" || corpusID != "default" || documentID != "doc-failed" || key != "retry-key" {
			t.Fatalf("retry scope/input=%q/%q/%q/%q", ownerID, corpusID, documentID, key)
		}
		return knowledge.CreateDocumentResult{
			DocumentID: documentID, JobID: "job-retry",
			TextIndexState: knowledge.TextIndexPending, VectorIndexState: knowledge.VectorIndexDisabled,
		}, nil
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	missing, err := http.Post(ts.URL+"/api/v1/knowledge/documents/doc-failed/retry?user_id=forged", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusBadRequest || stub.calls != 0 {
		t.Fatalf("missing key status=%d calls=%d", missing.StatusCode, stub.calls)
	}

	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/knowledge/documents/doc-failed/retry?user_id=forged", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "retry-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload knowledge.CreateDocumentResult
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted || payload.DocumentID != "doc-failed" ||
		payload.JobID != "job-retry" || stub.calls != 1 {
		t.Fatalf("status=%d payload=%+v calls=%d", resp.StatusCode, payload, stub.calls)
	}
}

func TestKnowledgeDocumentRetryMapsConflictReuploadAndMissing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "active conflict", err: knowledge.ErrIdempotencyConflict, want: http.StatusConflict, code: "knowledge_idempotency_conflict"},
		{name: "cancelled requires reupload", err: knowledge.ErrDocumentRetryRequiresReupload, want: http.StatusConflict, code: "knowledge_document_retry_requires_reupload"},
		{name: "not retryable", err: knowledge.ErrDocumentRetryNotAllowed, want: http.StatusConflict, code: "knowledge_document_retry_not_allowed"},
		{name: "missing", err: knowledge.ErrSemanticIndexNotFound, want: http.StatusNotFound, code: "semantic_index_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newRetryDocumentHTTPStub(t)
			stub.retryFn = func(context.Context, string, string, string, string) (knowledge.CreateDocumentResult, error) {
				return knowledge.CreateDocumentResult{}, test.err
			}
			srv := NewServer(config.DefaultConfig(), nil, nil, nil)
			srv.SetSemanticIndexService(stub)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/documents/doc/retry", nil)
			req.SetPathValue("id", "doc")
			req.Header.Set("Idempotency-Key", "retry-key")
			rec := httptest.NewRecorder()
			srv.handleRetryKnowledgeDocument(rec, req)
			var payload map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if rec.Code != test.want || payload["code"] != test.code {
				t.Fatalf("status=%d payload=%v", rec.Code, payload)
			}
		})
	}
}

func TestKnowledgeDocumentRetryDoesNotCollapseInternalFailureIntoConflict(t *testing.T) {
	stub := newRetryDocumentHTTPStub(t)
	stub.retryFn = func(context.Context, string, string, string, string) (knowledge.CreateDocumentResult, error) {
		return knowledge.CreateDocumentResult{}, errors.New("database unavailable")
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/documents/doc/retry", nil)
	req.SetPathValue("id", "doc")
	req.Header.Set("Idempotency-Key", "retry-key")
	rec := httptest.NewRecorder()
	srv.handleRetryKnowledgeDocument(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("internal retry status=%d body=%s", rec.Code, rec.Body.String())
	}
}
