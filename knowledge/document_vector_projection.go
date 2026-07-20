package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DocumentVectorProjection is the actionable per-document view of the
// current semantic target. Text readiness remains on Document; this projection
// exposes an embedding-only failure without falsely marking the upload failed.
type DocumentVectorProjection struct {
	DocumentID       string            `json:"document_id"`
	VectorIndexState VectorIndexState  `json:"vector_index_state"`
	JobID            string            `json:"vector_job_id,omitempty"`
	JobState         KnowledgeJobState `json:"vector_job_state,omitempty"`
	Stage            JobStage          `json:"vector_job_stage,omitempty"`
	ChunksDone       *int64            `json:"vector_chunks_done,omitempty"`
	ChunksTotal      *int64            `json:"vector_chunks_total,omitempty"`
	LastError        string            `json:"vector_error,omitempty"`
	OutcomeUnknown   bool              `json:"vector_outcome_unknown,omitempty"`
}

type documentVectorProjectionRepository interface {
	ListDocumentVectorProjections(context.Context, string, string) (map[string]DocumentVectorProjection, error)
}

func (s *SemanticIndexService) ListDocumentVectorProjections(
	ctx context.Context,
	ownerID, corpusID string,
) (map[string]DocumentVectorProjection, error) {
	repository, ok := s.repository.(documentVectorProjectionRepository)
	if !ok {
		return nil, ErrSemanticIndexNotFound
	}
	return repository.ListDocumentVectorProjections(ctx, ownerID, corpusID)
}

func (r *SQLiteSemanticIndexRepository) ListDocumentVectorProjections(
	ctx context.Context,
	ownerID, corpusID string,
) (map[string]DocumentVectorProjection, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT b.document_id,
		CASE
		  WHEN p.selection_kind='disabled' THEN 'disabled'
		  WHEN b.text_state IN ('pending','building') THEN 'pending'
		  WHEN rd.vector_state IS NULL THEN 'failed'
		  ELSE rd.vector_state
		END,
		COALESCE(j.job_id,''),COALESCE(j.state,''),COALESCE(j.stage,''),
		j.chunks_done,j.chunks_total,COALESCE(j.last_error,''),
		EXISTS(
		  SELECT 1 FROM kb_embedding_batch_manifests bm
		  WHERE bm.job_id=j.job_id AND bm.state='outcome_unknown'
		)
	FROM kb_semantic_corpora c
	JOIN kb_embedding_policies p ON p.corpus_uid=c.corpus_uid
	JOIN kb_semantic_document_bindings b ON b.corpus_uid=c.corpus_uid
	LEFT JOIN kb_revision_documents rd
	  ON rd.corpus_uid=b.corpus_uid
	 AND rd.revision_id=COALESCE(p.desired_revision_id,c.active_revision_id)
	 AND rd.document_id=b.document_id
	 AND rd.content_generation=b.content_generation
	LEFT JOIN kb_knowledge_jobs j ON j.job_id=(
	  SELECT candidate.job_id FROM kb_knowledge_jobs candidate
	  WHERE candidate.owner_id=b.owner_id AND candidate.corpus_uid=b.corpus_uid
	    AND candidate.document_id=b.document_id
	    AND candidate.document_generation=b.content_generation
	    AND candidate.kind='embed_document'
	    AND candidate.target_revision_id=COALESCE(p.desired_revision_id,c.active_revision_id)
	  ORDER BY candidate.created_at DESC,candidate.job_id DESC LIMIT 1
	)
	WHERE c.owner_id=? AND c.corpus_alias=?
	  AND b.owner_id=? AND b.lifecycle_state='active'`, ownerID, corpusID, ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]DocumentVectorProjection{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]DocumentVectorProjection{}
	for rows.Next() {
		var projection DocumentVectorProjection
		var vectorState, jobState, stage string
		var chunksDone, chunksTotal sql.NullInt64
		var outcomeUnknown int
		if err := rows.Scan(&projection.DocumentID, &vectorState, &projection.JobID,
			&jobState, &stage, &chunksDone, &chunksTotal, &projection.LastError,
			&outcomeUnknown); err != nil {
			return nil, err
		}
		projection.VectorIndexState = VectorIndexState(vectorState)
		if strings.TrimSpace(jobState) != "" {
			projection.JobState = KnowledgeJobState(jobState)
		}
		if strings.TrimSpace(stage) != "" {
			projection.Stage = JobStage(stage)
		}
		projection.ChunksDone = int64Pointer(chunksDone)
		projection.ChunksTotal = int64Pointer(chunksTotal)
		projection.OutcomeUnknown = outcomeUnknown != 0
		result[projection.DocumentID] = projection
	}
	return result, rows.Err()
}
