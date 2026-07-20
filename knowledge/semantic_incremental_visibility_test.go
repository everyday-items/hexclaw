package knowledge

import (
	"testing"
	"time"
)

func TestActiveEmbeddingFinalBatchRemainsInvisibleUntilJobCompletes(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_310_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-before-publish", now, time.Minute,
	)
	if err != nil || !ok || job.Kind != KnowledgeJobEmbedDocument {
		t.Fatalf("claim active embedding: job=%+v ok=%v err=%v", job, ok, err)
	}
	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, job.Lease(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(2*time.Second), nil, 10)
	if err != nil || len(inputs) != 2 {
		t.Fatalf("list inputs: len=%d err=%v", len(inputs), err)
	}
	texts := []string{inputs[0].Content, inputs[1].Content}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(
		h.ctx, job.Lease(), now.Add(3*time.Second),
		makeEmbeddingBatchManifest(job.JobID, plan, inputs, texts),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(3500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(4*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 2, ChunksTotal: 2,
		Vectors: []RevisionVector{
			vectorForInput(inputs[0], []float32{1, 0, 0}),
			vectorForInput(inputs[1], []float32{1, 0, 0}),
		},
	}); err != nil {
		t.Fatal(err)
	}

	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", &semanticExecutorRegistry{
		executors: map[string]*semanticExecutor{"profile-a": {dimension: 3}},
	})
	results, routeRan, err := searcher.Search(h.ctx, "alpha", 10, Filter{})
	if err != nil || !routeRan {
		t.Fatalf("pre-completion search: routeRan=%v results=%+v err=%v", routeRan, results, err)
	}
	if len(results) != 0 {
		t.Fatalf("final batch became visible before job completion: %+v", results)
	}

	if err := h.repo.CompleteActiveRevisionJob(
		h.ctx, job.Lease(), now.Add(5*time.Second), plan.ContentVersion,
	); err != nil {
		t.Fatal(err)
	}
	results, routeRan, err = searcher.Search(h.ctx, "alpha", 10, Filter{})
	if err != nil || !routeRan || len(results) != 2 {
		t.Fatalf("post-completion search: routeRan=%v len=%d results=%+v err=%v",
			routeRan, len(results), results, err)
	}
}

func TestActiveEmbeddingCancelAfterFinalBatchLeavesZeroVisibleHits(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_311_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-before-cancel", now, time.Minute,
	)
	if err != nil || !ok || job.Kind != KnowledgeJobEmbedDocument {
		t.Fatalf("claim active embedding: job=%+v ok=%v err=%v", job, ok, err)
	}
	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, job.Lease(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(2*time.Second), nil, 10)
	if err != nil || len(inputs) != 2 {
		t.Fatalf("list inputs: len=%d err=%v", len(inputs), err)
	}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(
		h.ctx, job.Lease(), now.Add(3*time.Second),
		makeEmbeddingBatchManifest(job.JobID, plan, inputs, []string{inputs[0].Content, inputs[1].Content}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(3500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(4*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 2, ChunksTotal: 2,
		Vectors: []RevisionVector{
			vectorForInput(inputs[0], []float32{1, 0, 0}),
			vectorForInput(inputs[1], []float32{1, 0, 0}),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", job.JobID); err != nil {
		t.Fatal(err)
	}

	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", &semanticExecutorRegistry{
		executors: map[string]*semanticExecutor{"profile-a": {dimension: 3}},
	})
	results, routeRan, err := searcher.Search(h.ctx, "alpha", 10, Filter{})
	if err != nil || !routeRan {
		t.Fatalf("post-cancel search: routeRan=%v results=%+v err=%v", routeRan, results, err)
	}
	if len(results) != 0 {
		t.Fatalf("cancelled final batch remained query-visible: %+v", results)
	}
	var state string
	var visibleAt any
	if err := h.db.QueryRowContext(h.ctx, `SELECT vector_state,visible_at
		FROM kb_revision_documents WHERE revision_id=? AND document_id=? AND content_generation=1`,
		h.active, doc.ID).Scan(&state, &visibleAt); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || visibleAt != nil {
		t.Fatalf("cancelled projection state=%q visible_at=%v, want cancelled/NULL", state, visibleAt)
	}
}
