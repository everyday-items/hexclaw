package knowledge

import (
	"errors"
	"testing"
	"time"
)

func TestSemanticIndexWorkerPlanBatchCommitAndActivePlan(t *testing.T) {
	h := newSemanticHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-1','Legacy','body',0)`); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"alpha chunk", "beta chunk"} {
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,?,NULL)`, []string{"chunk-a", "chunk-b"}[i], "doc-1", content, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_002_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: job=%+v ok=%v err=%v", job, ok, err)
	}
	lease := job.Lease()

	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, lease, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if plan.CorpusAlias != "default" || plan.RevisionID != *staged.DesiredRevisionID ||
		plan.PolicyVersion != staged.PolicyVersion || plan.ContentVersion != 1 ||
		plan.Snapshot.ProfileConfigHash != "hash-a" || plan.Snapshot.Profile.Dimension != 3 {
		t.Fatalf("execution plan did not freeze target revision facts: %+v", plan)
	}

	pageOne, err := h.repo.ListRevisionChunkInputs(h.ctx, lease, now.Add(2*time.Second), nil, 1)
	if err != nil || len(pageOne) != 1 {
		t.Fatalf("first chunk page: len=%d err=%v", len(pageOne), err)
	}
	pageTwo, err := h.repo.ListRevisionChunkInputs(h.ctx, lease, now.Add(3*time.Second), &pageOne[0].Cursor, 10)
	if err != nil || len(pageTwo) != 1 {
		t.Fatalf("second chunk page: len=%d err=%v", len(pageTwo), err)
	}
	inputs := append(pageOne, pageTwo...)
	if inputs[0].ChunkID == inputs[1].ChunkID || inputs[0].ContentHash == "" || inputs[1].ContentHash == "" {
		t.Fatalf("chunk inputs are not stable/hash-addressed: %+v", inputs)
	}

	manifest, err := h.repo.CreateEmbeddingBatchManifest(h.ctx, lease, now.Add(4*time.Second), EmbeddingBatchManifest{
		ChunkIDsDigest: "chunk-ids-v1", PayloadDigest: "payload-v1", ClientRequestKey: "request-v1",
		Chunks: []EmbeddingBatchChunk{
			{Ordinal: 0, ChunkID: inputs[0].ChunkID, ContentHash: inputs[0].ContentHash},
			{Ordinal: 1, ChunkID: inputs[1].ChunkID, ContentHash: inputs[1].ContentHash},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, lease, now.Add(4500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	wrong := EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 2, ChunksTotal: 2,
		Vectors: []RevisionVector{
			vectorForInput(inputs[0], []float32{1, 0}),
			vectorForInput(inputs[1], []float32{0, 1}),
		},
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, lease, now.Add(5*time.Second), wrong); !errors.Is(err, ErrInvalidRevisionVector) {
		t.Fatalf("wrong dimension error = %v, want ErrInvalidRevisionVector", err)
	}
	var vectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 0 {
		t.Fatalf("invalid batch wrote vectors=%d err=%v", vectors, err)
	}

	commit := EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 2, ChunksTotal: 2,
		ProviderRequestID: "provider-request-1",
		Vectors: []RevisionVector{
			vectorForInput(inputs[0], []float32{1, 0, 0}),
			vectorForInput(inputs[1], []float32{0, 1, 0}),
		},
		Checkpoint: &StageCheckpoint{
			Stage: JobStageEmbedding, InputFingerprint: "input-v1",
			ArtifactRef: "local://batch-1", ArtifactDigest: "artifact-v1",
			State: StageCheckpointSucceeded,
		},
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, lease, now.Add(6*time.Second), commit); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=? AND profile_config_hash='hash-a' AND provider_id='ollama'
		  AND model_name='bge-m3' AND dimension=3 AND length(embedding)=12`, plan.RevisionID).Scan(&vectors); err != nil || vectors != 2 {
		t.Fatalf("revision-scoped vector rows=%d err=%v, want 2", vectors, err)
	}
	var legacyVectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks WHERE embedding IS NOT NULL`).Scan(&legacyVectors); err != nil || legacyVectors != 0 {
		t.Fatalf("new pipeline wrote legacy kb_chunks.embedding rows=%d err=%v", legacyVectors, err)
	}

	summary, err := h.repo.GetRevisionBuildSummary(h.ctx, lease, now.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ExpectedChunks != 2 || summary.EmbeddedChunks != 2 || summary.FailedChunks != 0 || summary.ChunkSetDigest == "" {
		t.Fatalf("unexpected revision build summary: %+v", summary)
	}
	if err := h.repo.PrepareRevisionForPublish(h.ctx, lease, now.Add(8*time.Second), RevisionPublishPreparation{
		IndexedThroughVersion: plan.ContentVersion,
		ChunkSetDigest:        summary.ChunkSetDigest,
		ExpectedChunks:        summary.ExpectedChunks,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
		Lease: lease, Now: now.Add(9 * time.Second), OwnerID: "owner-1", CorpusID: "default", RevisionID: plan.RevisionID,
		ExpectedPolicyVersion: staged.PolicyVersion, ExpectedContentVersion: plan.ContentVersion,
	}); err != nil {
		t.Fatal(err)
	}
	active, err := h.repo.GetActiveRevisionPlan(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if active.RevisionID != plan.RevisionID || active.Snapshot.ProfileConfigHash != "hash-a" ||
		active.Snapshot.Profile.Dimension != 3 {
		t.Fatalf("active query plan = %+v", active)
	}
}

func TestSemanticIndexCancelledLeaseCommitsZeroVectors(t *testing.T) {
	h := newSemanticHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-1','Legacy','body',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('chunk-a','doc-1','alpha',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	_, _ = h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	staged, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	now := time.Unix(1_800_003_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(time.Second), nil, 10)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("inputs: len=%d err=%v", len(inputs), err)
	}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(h.ctx, job.Lease(), now.Add(2*time.Second), EmbeddingBatchManifest{
		ChunkIDsDigest: "chunks", PayloadDigest: "payload", ClientRequestKey: "request-cancel",
		Chunks: []EmbeddingBatchChunk{{Ordinal: 0, ChunkID: inputs[0].ChunkID, ContentHash: inputs[0].ContentHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(2500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(3*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 1, ChunksTotal: 1,
		Vectors: []RevisionVector{vectorForInput(inputs[0], []float32{1, 0, 0})},
	}); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("cancelled commit error = %v, want ErrJobFenced", err)
	}
	var vectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 0 {
		t.Fatalf("cancelled lease wrote vectors=%d err=%v", vectors, err)
	}
}

func TestSemanticIndexCompleteActiveDocumentJobIsFencedAndDoesNotRepublish(t *testing.T) {
	h := newSemanticHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil || boot.ActiveRevisionID == nil {
		t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-incremental','Incremental','alpha',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('chunk-incremental','doc-incremental','alpha',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	binding, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	if err != nil || binding.ContentVersion != 1 {
		t.Fatalf("reconcile active document: binding=%+v err=%v", binding, err)
	}
	now := time.Unix(1_800_003_500, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-active", now, time.Minute,
	)
	if err != nil || !ok || job.Kind != KnowledgeJobEmbedDocument {
		t.Fatalf("claim active document: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_revision_documents
		SET vector_state='building',embedded_chunks=expected_chunks,visible_at=NULL,updated_at=?
		WHERE revision_id=? AND document_id='doc-incremental' AND content_generation=1`,
		now.Add(time.Second).UnixMilli(), *boot.ActiveRevisionID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CompleteActiveRevisionJob(
		h.ctx, job.Lease(), now.Add(2*time.Second), binding.ContentVersion,
	); err != nil {
		t.Fatal(err)
	}
	completed, err := h.service.GetJob(h.ctx, "owner-1", job.JobID)
	if err != nil || completed.State != KnowledgeJobSucceeded {
		t.Fatalf("completed active document job=%+v err=%v", completed, err)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.ActiveRevision == nil ||
		projection.ActiveRevision.RevisionID != *boot.ActiveRevisionID || projection.DesiredRevision != nil {
		t.Fatalf("active completion changed policy pointer: projection=%+v err=%v", projection, err)
	}
	var indexedThrough int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT indexed_through_version FROM kb_index_revisions
		WHERE revision_id=?`, *boot.ActiveRevisionID).Scan(&indexedThrough); err != nil || indexedThrough != 1 {
		t.Fatalf("indexed-through version=%d err=%v, want 1", indexedThrough, err)
	}
	if err := h.repo.CompleteActiveRevisionJob(
		h.ctx, job.Lease(), now.Add(3*time.Second), binding.ContentVersion,
	); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("replayed stale lease error=%v, want ErrJobFenced", err)
	}
}

func TestSemanticIndexRetryAndFailAreDurableFencedTransitions(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	_, _ = h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	now := time.Unix(1_800_004_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	next := now.Add(5 * time.Minute)
	retrying, err := h.repo.RetryJob(h.ctx, job.Lease(), now.Add(time.Second), next, "rate_limited")
	if err != nil {
		t.Fatal(err)
	}
	if retrying.State != KnowledgeJobRetryWait || retrying.NextAttemptAt == nil || retrying.LeaseEpoch <= job.LeaseEpoch {
		t.Fatalf("retry transition = %+v", retrying)
	}
	if _, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "too-early", next.Add(-time.Second), time.Minute); err != nil || ok {
		t.Fatalf("retry claimed before next_attempt_at: ok=%v err=%v", ok, err)
	}
	reclaimed, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-2", next, time.Minute)
	if err != nil || !ok {
		t.Fatalf("retry reclaim: ok=%v err=%v", ok, err)
	}
	failed, err := h.repo.FailJob(h.ctx, reclaimed.Lease(), next.Add(time.Second), "provider_rejected")
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != KnowledgeJobFailed || failed.LastError != "provider_rejected" || failed.LeaseEpoch <= reclaimed.LeaseEpoch {
		t.Fatalf("failed transition = %+v", failed)
	}
	var publishState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT publish_state FROM kb_index_revisions
		WHERE revision_id=?`, failed.TargetRevisionID).Scan(&publishState); err != nil {
		t.Fatal(err)
	}
	if publishState != "abandoned" {
		t.Fatalf("failed desired root revision state=%q, want abandoned", publishState)
	}
	childTime := next.Add(2 * time.Second).UnixMilli()
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,target_revision_id,
		 idempotency_key,state,stage,attempt,cancel_requested,lease_owner,lease_epoch,
		 last_error,created_at,updated_at)
		VALUES('future-child',?,'download_model','owner-1',?,NULL,?,'future-child',
		 'queued','embedding',0,0,'',0,'',?,?)`, failed.JobID, failed.CorpusUID,
		failed.TargetRevisionID, childTime, childTime); err != nil {
		t.Fatal(err)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if projection.IndexingActivity.State != IndexingActivityFailed || projection.ActiveRevision == nil ||
		projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil ||
		*projection.DesiredRevision.JobID != failed.JobID {
		t.Fatalf("failed staged job must keep old active and project failed: %+v", projection)
	}
}

func vectorForInput(input RevisionChunkInput, values []float32) RevisionVector {
	return RevisionVector{
		DocumentID: input.DocumentID, ContentGeneration: input.ContentGeneration,
		ChunkID: input.ChunkID, ChunkIndex: input.ChunkIndex,
		ContentHash: input.ContentHash, Values: values,
	}
}
