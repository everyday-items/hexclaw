package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/logger"
)

const (
	knowledgeDesktopPrincipalID = "desktop-user"
	knowledgeDefaultCorpusID    = "default"
)

func knowledgePrincipalID(r *http.Request) string {
	if r != nil {
		if ownerID := strings.TrimSpace(skill.AuthenticatedUserID(r.Context())); ownerID != "" {
			return ownerID
		}
	}
	// Legacy direct-handler/embedded callers are desktop-local. Production HTTP
	// always passes through apiAuthMiddleware and therefore uses the principal
	// above; query/body user_id is intentionally never consulted.
	return knowledgeDesktopPrincipalID
}

func requireSupportedKnowledgeCorpus(w http.ResponseWriter, corpusID string) bool {
	if corpusID == knowledgeDefaultCorpusID {
		return true
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
		"error": "only the default desktop knowledge corpus is supported",
		"code":  "knowledge_scope_unsupported",
	})
	return false
}

// SemanticIndexAPI is the narrow application boundary exposed over HTTP. The
// worker-only claim/checkpoint/publish commands deliberately do not belong to
// this interface and therefore cannot become accidental public routes.
type SemanticIndexAPI interface {
	GetPolicy(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error)
	ApplyPolicy(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error)
	GetJobForCorpus(context.Context, string, string, string) (knowledge.KnowledgeJob, error)
	CancelJobForCorpus(context.Context, string, string, string) (knowledge.KnowledgeJob, error)
}

type semanticIndexAPI = SemanticIndexAPI

// KnowledgeDocumentIngestAPI is intentionally separate from policy/job reads
// so existing semantic-index test doubles and non-upload runtimes do not gain
// an accidental file-system capability.
type KnowledgeDocumentIngestAPI interface {
	CreateDocument(context.Context, string, string, knowledge.CreateDocumentInput) (knowledge.CreateDocumentResult, error)
}

// KnowledgeDocumentRetryAPI is a separate command boundary because retry
// consumes an already-durable source and must never fall back to the legacy
// empty-body reindex path.
type KnowledgeDocumentRetryAPI interface {
	RetryDocument(context.Context, string, string, string, string) (knowledge.CreateDocumentResult, error)
}

// KnowledgeDocumentProjectionAPI exposes only the durable async document
// projection needed by GET detail. Legacy-only runtimes continue to fall back
// to Manager.GetDocument without implementing this interface.
type KnowledgeDocumentProjectionAPI interface {
	GetIngestDocumentProjectionForCorpus(context.Context, string, string, string) (knowledge.KnowledgeDocumentProjection, error)
}

type KnowledgeOperationProjectionAPI interface {
	ListUploadOperationsForCorpus(
		context.Context, string, string,
	) ([]knowledge.UploadOperationProjection, error)
}

type KnowledgeUploadResponseAcknowledger interface {
	MarkUploadResponseDelivered(context.Context, string, string, string) error
}

// KnowledgeDocumentVectorProjectionAPI enriches list/detail rows with the
// current generation's embedding-only lifecycle and actionable child Job.
type KnowledgeDocumentVectorProjectionAPI interface {
	ListDocumentVectorProjections(context.Context, string, string) (map[string]knowledge.DocumentVectorProjection, error)
}

// SetSemanticIndexService installs the durable semantic-index application
// service. Keeping this separate from SetKnowledgeBase allows the policy/job
// routes to remain explicit and independently testable.
func (s *Server) SetSemanticIndexService(service SemanticIndexAPI) {
	s.semanticIndex = service
}

func (s *Server) handleGetKnowledgeEmbeddingPolicy(w http.ResponseWriter, r *http.Request) {
	corpusID := strings.TrimSpace(r.PathValue("corpus_id"))
	if corpusID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "corpus_id is required"})
		return
	}
	if !requireSupportedKnowledgeCorpus(w, corpusID) {
		return
	}
	projection, err := s.semanticIndex.GetPolicy(r.Context(), knowledgePrincipalID(r), corpusID)
	if err != nil {
		writeSemanticIndexError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (s *Server) handleApplyKnowledgeEmbeddingPolicy(w http.ResponseWriter, r *http.Request) {
	corpusID := strings.TrimSpace(r.PathValue("corpus_id"))
	if corpusID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "corpus_id is required"})
		return
	}
	if !requireSupportedKnowledgeCorpus(w, corpusID) {
		return
	}
	var req struct {
		ExpectedPolicyVersion *int64                       `json:"expected_policy_version"`
		Selection             knowledge.EmbeddingSelection `json:"selection"`
	}
	if err := decodeSemanticIndexRequest(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.ExpectedPolicyVersion == nil || *req.ExpectedPolicyVersion < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected_policy_version is required"})
		return
	}
	if err := req.Selection.Validate(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	result, err := s.semanticIndex.ApplyPolicy(
		r.Context(), knowledgePrincipalID(r), corpusID,
		*req.ExpectedPolicyVersion, req.Selection,
	)
	if err != nil {
		writeSemanticIndexError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetKnowledgeJob(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.PathValue("job_id"))
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job_id is required"})
		return
	}
	job, err := s.semanticIndex.GetJobForCorpus(
		r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID, jobID,
	)
	if err != nil {
		writeSemanticIndexError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelKnowledgeJob(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.PathValue("job_id"))
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job_id is required"})
		return
	}
	job, err := s.semanticIndex.CancelJobForCorpus(
		r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID, jobID,
	)
	if err != nil {
		writeSemanticIndexError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleRetryKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	documentID := strings.TrimSpace(r.PathValue("id"))
	if documentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "document_id is required", "code": "knowledge_document_retry_invalid",
		})
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Idempotency-Key is required", "code": "knowledge_document_retry_invalid",
		})
		return
	}
	service, ok := s.semanticIndex.(KnowledgeDocumentRetryAPI)
	if !ok {
		writeDocumentIngestError(w, knowledge.ErrDocumentIngestUnavailable)
		return
	}
	result, err := service.RetryDocument(
		r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID, documentID, idempotencyKey,
	)
	if err != nil {
		writeDocumentRetryError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func writeDocumentRetryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrInvalidDocumentRetry):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "knowledge_document_retry_invalid"})
	case errors.Is(err, knowledge.ErrIdempotencyConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "knowledge_idempotency_conflict"})
	case errors.Is(err, knowledge.ErrDocumentRetryRequiresReupload):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "knowledge_document_retry_requires_reupload"})
	case errors.Is(err, knowledge.ErrDocumentRetryNotAllowed):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "knowledge_document_retry_not_allowed"})
	case errors.Is(err, knowledge.ErrSemanticIndexNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error(), "code": "semantic_index_not_found"})
	case errors.Is(err, knowledge.ErrDocumentIngestUnavailable):
		writeDocumentIngestError(w, err)
	default:
		logger.Error("[knowledge] document retry failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "knowledge document retry failed", "code": "knowledge_document_retry_internal",
		})
	}
}

func decodeSemanticIndexRequest(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid JSON: multiple values")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeSemanticIndexError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := err.Error()
	code := "semantic_index_internal"
	switch {
	case errors.Is(err, knowledge.ErrInvalidSelection), errors.Is(err, knowledge.ErrInvalidEmbeddingProfile),
		errors.Is(err, knowledge.ErrProfileUnavailable):
		status = http.StatusUnprocessableEntity
		code = "semantic_index_invalid_profile"
	case errors.Is(err, knowledge.ErrPolicyVersionConflict):
		status = http.StatusConflict
		code = "semantic_index_version_conflict"
	case errors.Is(err, knowledge.ErrSemanticIndexNotFound):
		status = http.StatusNotFound
		code = "semantic_index_not_found"
	}
	if status == http.StatusInternalServerError {
		logger.Error("[knowledge] semantic index request failed", "error", err)
		message = "semantic index temporarily unavailable"
	}
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}
