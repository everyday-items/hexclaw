package knowledge

import (
	"testing"
	"time"
)

func TestListDocumentVectorProjectionsExposesActiveEmbeddingFailureAndJob(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_330_000, 0).UTC()
	config := workerConfig(&now, "worker-vector-failure", 16)
	config.MaxAttempts = 1
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{
			"profile-a": &scriptedWorkerExecutor{dimension: 3, failAll: true},
		},
	}, config)
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || runErr == nil {
		t.Fatalf("fail embedding: processed=%v err=%v", processed, runErr)
	}

	projections, err := h.repo.ListDocumentVectorProjections(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	projection, ok := projections[doc.ID]
	if !ok {
		t.Fatalf("missing projection for %s: %+v", doc.ID, projections)
	}
	if projection.VectorIndexState != VectorIndexFailed || projection.JobID == "" ||
		projection.JobState != KnowledgeJobFailed || projection.Stage != JobStageEmbedding ||
		projection.LastError == "" || projection.OutcomeUnknown {
		t.Fatalf("failed vector projection=%+v", projection)
	}
	if projection.ChunksDone == nil || projection.ChunksTotal == nil ||
		*projection.ChunksDone != 0 || *projection.ChunksTotal != 2 {
		t.Fatalf("failed vector progress=%v/%v", projection.ChunksDone, projection.ChunksTotal)
	}

	listed, err := h.store.List(h.ctx)
	if err != nil || len(listed) != 1 || listed[0].Status != "indexed" {
		t.Fatalf("embedding-only failure changed text document status: docs=%+v err=%v", listed, err)
	}
}
