package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

type uploadOperationHTTPStub struct {
	semanticIndexServiceStub
	operations []knowledge.UploadOperationProjection
	ownerID    string
	corpusID   string
	calls      int
	ackOwner   string
	ackCorpus  string
	ackID      string
	ackCalls   int
}

func (s *uploadOperationHTTPStub) ListUploadOperationsForCorpus(
	_ context.Context, ownerID, corpusID string,
) ([]knowledge.UploadOperationProjection, error) {
	s.calls++
	s.ownerID, s.corpusID = ownerID, corpusID
	return append([]knowledge.UploadOperationProjection(nil), s.operations...), nil
}

func (s *uploadOperationHTTPStub) MarkUploadResponseDelivered(
	_ context.Context, ownerID, corpusID, operationID string,
) error {
	s.ackCalls++
	s.ackOwner, s.ackCorpus, s.ackID = ownerID, corpusID, operationID
	return nil
}

func TestKnowledgeOperationProjectionRequiresPrincipalAndUsesOwnerCorpusScope(t *testing.T) {
	now := time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC)
	stub := &uploadOperationHTTPStub{operations: []knowledge.UploadOperationProjection{{
		OperationID: "upload_immutable", DocumentID: "doc_immutable", JobID: "job_immutable",
		DisplayName: "六上数学.pdf", ContentDigest: strings.Repeat("a", 64),
		State: knowledge.UploadOperationRunning, Stage: string(knowledge.JobStageOCR),
		Terminal: false, UpdatedAt: now,
	}}}
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "api-token"
	srv := NewServer(cfg, nil, nil, nil)
	srv.SetSemanticIndexService(stub)
	handler := srv.routes()

	request := func(token, corpus string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"http://example.test/api/v1/knowledge/operations?owner_id=forged&corpus_id="+corpus, nil)
		req.RemoteAddr = "203.0.113.10:4040"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := request("", "default"); rec.Code != http.StatusUnauthorized || stub.calls != 0 {
		t.Fatalf("anonymous status=%d calls=%d body=%s", rec.Code, stub.calls, rec.Body.String())
	}
	rec := request("api-token", "default")
	if rec.Code != http.StatusOK || stub.ownerID != "api-user" || stub.corpusID != "default" || stub.calls != 1 {
		t.Fatalf("authorized status=%d scope=%s/%s calls=%d body=%s",
			rec.Code, stub.ownerID, stub.corpusID, stub.calls, rec.Body.String())
	}
	var payload struct {
		Operations []struct {
			OperationID   string                         `json:"operation_id"`
			DocumentID    string                         `json:"document_id"`
			JobID         string                         `json:"job_id"`
			Title         string                         `json:"title"`
			DisplayName   string                         `json:"display_name"`
			ContentDigest string                         `json:"content_digest"`
			State         knowledge.UploadOperationState `json:"state"`
			Stage         string                         `json:"stage"`
			Terminal      bool                           `json:"terminal"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Operations) != 1 {
		t.Fatalf("operations=%+v", payload.Operations)
	}
	got := payload.Operations[0]
	if got.OperationID != "upload_immutable" || got.DocumentID != "doc_immutable" ||
		got.JobID != "job_immutable" || got.Title != "六上数学.pdf" ||
		got.DisplayName != "六上数学.pdf" || got.ContentDigest != strings.Repeat("a", 64) ||
		got.State != knowledge.UploadOperationRunning || got.Stage != string(knowledge.JobStageOCR) || got.Terminal {
		t.Fatalf("durable operation contract drift: %+v", got)
	}

	if rec := request("api-token", "other"); rec.Code != http.StatusUnprocessableEntity || stub.calls != 1 {
		t.Fatalf("cross-corpus status=%d calls=%d body=%s", rec.Code, stub.calls, rec.Body.String())
	}

	ack := func(token, corpus string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost,
			"http://example.test/api/v1/knowledge/operations/upload_immutable/ack?corpus_id="+corpus, nil)
		req.RemoteAddr = "203.0.113.10:4040"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := ack("", "default"); rec.Code != http.StatusUnauthorized || stub.ackCalls != 0 {
		t.Fatalf("anonymous ack status=%d calls=%d body=%s", rec.Code, stub.ackCalls, rec.Body.String())
	}
	if rec := ack("api-token", "default"); rec.Code != http.StatusNoContent ||
		stub.ackCalls != 1 || stub.ackOwner != "api-user" || stub.ackCorpus != "default" ||
		stub.ackID != "upload_immutable" {
		t.Fatalf("ack status=%d scope=%s/%s id=%s calls=%d body=%s", rec.Code,
			stub.ackOwner, stub.ackCorpus, stub.ackID, stub.ackCalls, rec.Body.String())
	}
	if rec := ack("api-token", "other"); rec.Code != http.StatusUnprocessableEntity || stub.ackCalls != 1 {
		t.Fatalf("cross-corpus ack status=%d calls=%d body=%s", rec.Code, stub.ackCalls, rec.Body.String())
	}
}
