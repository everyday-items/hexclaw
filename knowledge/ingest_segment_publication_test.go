package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func TestIngestSegmentReadinessSurvivesRestartAndIdempotentReplay(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	if err := migrate.Run(ctx, db, []migrate.Migration{migrate.KnowledgeIngestExecutionV46}); err != nil {
		t.Fatal(err)
	}
	body := "%PDF-1.7\nsegment restart fixture"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "segment-publication-restart",
		Filename:       "segmented.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repository.ClaimNextJobForCorpus(
		ctx, "desktop-user", "default", "segment-worker", now, time.Minute,
	)
	if err != nil || !claimed || job.JobID != accepted.JobID {
		t.Fatalf("claim job=%+v claimed=%v err=%v", job, claimed, err)
	}
	source, err := repository.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 4); err != nil {
		t.Fatal(err)
	}
	segments := []IngestSegmentPlan{
		{Ordinal: 1, PageStart: 1, PageEnd: 2, Mode: IngestSegmentVisual},
		{Ordinal: 2, PageStart: 3, PageEnd: 4, Mode: IngestSegmentVisual},
	}
	if err := repository.SaveIngestSegmentPlan(
		ctx, job.Lease(), now, source.SHA256, segments,
	); err != nil {
		t.Fatal(err)
	}
	assertSegmentStates(t, db, accepted.JobID, []string{"planned", "planned"})

	if err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 4, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "segment page one",
	}); err != nil {
		t.Fatal(err)
	}
	assertSegmentStates(t, db, accepted.JobID, []string{"processing", "planned"})
	if err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 2, PagesTotal: 4, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "segment page two",
	}); err != nil {
		t.Fatal(err)
	}
	assertSegmentStates(t, db, accepted.JobID, []string{"ready", "planned"})

	restarted := NewSQLiteSemanticIndexRepository(db)
	if err := restarted.SaveIngestSegmentPlan(
		ctx, job.Lease(), now, source.SHA256, segments,
	); err != nil {
		t.Fatalf("restart plan replay: %v", err)
	}
	if err := restarted.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 2, PagesTotal: 4, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "segment page two",
	}); err != nil {
		t.Fatalf("restart checkpoint replay: %v", err)
	}
	assertSegmentStates(t, db, accepted.JobID, []string{"ready", "planned"})

	var segmentRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_segments
		WHERE job_id=?`, accepted.JobID).Scan(&segmentRows); err != nil {
		t.Fatal(err)
	}
	if segmentRows != 2 {
		t.Fatalf("idempotent plan replay rows=%d want=2", segmentRows)
	}
}

func assertSegmentStates(t *testing.T, db queryRower, jobID string, want []string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT state FROM kb_ingest_segments
		WHERE job_id=? ORDER BY ordinal`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		got = append(got, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("segment states=%v want=%v", got, want)
	}
}

func TestSegmentAwareSearchReturnsAllAndOnlyReadySegmentsWithParentCitation(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if err := migrate.Run(h.ctx, h.db, []migrate.Migration{migrate.KnowledgeIngestExecutionV46}); err != nil {
		t.Fatal(err)
	}
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}

	const documentID = "segment-parent-document"
	const parentTitle = "义务教育教科书·数学五年级下册"
	now := time.Unix(1_800_500_000, 0).UTC()
	sourceDigest := strings.Repeat("a", 64)
	document := &Document{
		ID: documentID, Title: parentTitle, Content: "one logical document",
		Source: "upload:textbook.pdf", SourceType: "upload", ChunkCount: 3,
		Status: "indexed", CreatedAt: now, UpdatedAt: now,
	}
	chunks := []*Chunk{
		{ID: "segment-ready-chunk", DocID: documentID, Content: "segmenttoken ready evidence",
			Index: 0, PageStart: 1, PageEnd: 2, SourceDigest: sourceDigest, CreatedAt: now},
		{ID: "segment-pending-chunk", DocID: documentID, Content: "segmenttoken pending evidence",
			Index: 1, PageStart: 3, PageEnd: 4, SourceDigest: sourceDigest, CreatedAt: now},
		{ID: "segment-failed-chunk", DocID: documentID, Content: "segmenttoken failed evidence",
			Index: 2, PageStart: 5, PageEnd: 6, SourceDigest: sourceDigest, CreatedAt: now},
	}
	if err := h.store.Add(h.ctx, document, chunks); err != nil {
		t.Fatal(err)
	}
	h.bindDocument("owner-1", corpusUID, documentID)
	seedSegmentedRevisionFixture(
		t, h, *boot.ActiveRevisionID, corpusUID, documentID, sourceDigest, chunks,
	)

	executor := &semanticExecutor{dimension: 3, vector: []float32{1, 0, 0}}
	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}},
	)
	results, ran, err := searcher.Search(h.ctx, "segment query", 10, Filter{})
	if err != nil || !ran {
		t.Fatalf("vector search ran=%v err=%v", ran, err)
	}
	assertReadySegmentCitation(t, results, parentTitle)
	textResults, err := searcher.TextSearch(h.ctx, "segmenttoken", 10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	assertReadySegmentCitation(t, textResults, parentTitle)

	restarted := NewSQLiteRevisionSemanticSearcher(
		h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3, vector: []float32{1, 0, 0}},
		}},
	)
	restartedResults, ran, err := restarted.Search(h.ctx, "segment query after restart", 10, Filter{})
	if err != nil || !ran {
		t.Fatalf("restart vector search ran=%v err=%v", ran, err)
	}
	assertReadySegmentCitation(t, restartedResults, parentTitle)
}

func assertReadySegmentCitation(t *testing.T, results []*SearchResult, parentTitle string) {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("ready-only results=%v want one", resultChunkIDs(results))
	}
	chunk := results[0].Chunk
	if chunk.ID != "segment-ready-chunk" || chunk.DocID != "segment-parent-document" ||
		chunk.DocTitle != parentTitle || chunk.PageStart != 1 || chunk.PageEnd != 2 {
		t.Fatalf("ready citation=%+v", chunk)
	}
}

func resultChunkIDs(results []*SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		if result != nil && result.Chunk != nil {
			ids = append(ids, result.Chunk.ID)
		}
	}
	return ids
}

func seedSegmentedRevisionFixture(
	t *testing.T,
	h *revisionSearchHarness,
	revisionID, corpusUID, documentID, sourceDigest string,
	chunks []*Chunk,
) {
	t.Helper()
	now := time.Unix(1_800_500_001, 0).UnixMilli()
	const jobID = "segment-root-job"
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,lease_owner,
		 lease_epoch,last_error,created_at,updated_at,finished_at)
		VALUES(?,NULL,'ingest','owner-1',?,?,1,NULL,?,'succeeded','text_indexing',
		 0,0,'',0,'',?,?,?)`, jobID, corpusUID, documentID, "segment-search-fixture",
		now, now, now); err != nil {
		t.Fatal(err)
	}
	states := []string{"ready", "processing", "failed"}
	for index, chunk := range chunks {
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_ingest_segments
			(segment_id,job_id,document_id,document_generation,ordinal,page_start,page_end,
			 extraction_mode,state,source_digest,plan_digest,last_error,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,'text',?,?,?,'',?,?)`,
			"segment-fixture-"+states[index], jobID, documentID, 1, index+1,
			chunk.PageStart, chunk.PageEnd, states[index], sourceDigest,
			strings.Repeat("b", 64), now, now); err != nil {
			t.Fatal(err)
		}
	}

	var snapshotID, profileHash, providerID, location, model string
	var dimension int
	if err := h.db.QueryRowContext(h.ctx, `SELECT s.profile_snapshot_id,s.profile_config_hash,
		s.provider_id,s.provider_location,s.model_name,s.dimension
		FROM kb_index_revisions r JOIN kb_embedding_profile_snapshots s
		  ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE r.revision_id=? AND r.corpus_uid=?`, revisionID, corpusUID).Scan(
		&snapshotID, &profileHash, &providerID, &location, &model, &dimension,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_revision_documents
		(revision_id,corpus_uid,document_id,content_generation,vector_state,
		 expected_chunks,embedded_chunks,failed_chunks,visible_at,updated_at)
		VALUES(?,?,?,1,'ready',3,3,0,?,?)`,
		revisionID, corpusUID, documentID, now, now); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		contentHash := sha256.Sum256([]byte(chunk.Content))
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_revision_vectors
			(revision_id,corpus_uid,document_id,content_generation,chunk_id,chunk_index,
			 chunk_content_hash,profile_snapshot_id,profile_config_hash,provider_id,
			 provider_location,model_name,dimension,embedding,created_at)
			VALUES(?,?,?,1,?,?,?,?,?,?,?,?,?,?,?)`,
			revisionID, corpusUID, documentID, chunk.ID, chunk.Index,
			hex.EncodeToString(contentHash[:]), snapshotID, profileHash, providerID,
			location, model, dimension, encodeFloat32Slice([]float32{1, 0, 0}), now,
		); err != nil {
			t.Fatal(err)
		}
	}
}
