package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCompletePDFIngestRequiresExactDurablePageCheckpointSet(t *testing.T) {
	tests := []struct {
		name          string
		checkpointSet []int
		manifestTotal int64
		corruptDigest bool
		wantError     bool
	}{
		{name: "zero checkpoints", manifestTotal: 3, wantError: true},
		{name: "missing first page", checkpointSet: []int{2, 3}, manifestTotal: 3, wantError: true},
		{name: "missing middle page", checkpointSet: []int{1, 3}, manifestTotal: 3, wantError: true},
		{name: "checkpoint page total differs", checkpointSet: []int{1, 2, 3}, manifestTotal: 4, wantError: true},
		{name: "checkpoint source digest differs", checkpointSet: []int{1, 2, 3}, manifestTotal: 3, corruptDigest: true, wantError: true},
		{name: "complete exact set", checkpointSet: []int{1, 2, 3}, manifestTotal: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, service, ctx := newAsyncIngestHarness(t)
			body := "%PDF-1.7\nthree durable pages"
			accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
				IdempotencyKey: "pdf-complete-" + strings.ReplaceAll(tt.name, " ", "-"),
				Filename:       "three-pages.pdf",
				MediaType:      "application/pdf",
				SizeBytes:      int64(len(body)),
				Body:           strings.NewReader(body),
			})
			if err != nil {
				t.Fatal(err)
			}
			repo := NewSQLiteSemanticIndexRepository(db)
			now := time.Now().UTC()
			job, claimed, err := repo.ClaimNextJobForCorpus(
				ctx, "desktop-user", "default", "pdf-completion-worker", now, time.Minute,
			)
			if err != nil || !claimed {
				t.Fatalf("claim=%v err=%v", claimed, err)
			}
			source, err := repo.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, tt.manifestTotal); err != nil {
				t.Fatal(err)
			}
			for _, page := range tt.checkpointSet {
				if err := repo.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
					PageNumber: page, PagesTotal: tt.manifestTotal, SourceDigest: source.SHA256,
					ExtractionMode: "ocr_vlm", Content: fmt.Sprintf("page %d content", page),
					OCRRouteReceipt: testOCRRouteReceipt(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.corruptDigest {
				if _, err := db.ExecContext(ctx, `UPDATE kb_ingest_page_checkpoints
					SET source_digest=? WHERE job_id=?`, strings.Repeat("f", 64), job.JobID); err != nil {
					t.Fatal(err)
				}
			}

			prepared := preparedPDFCompletionDocument(accepted.DocumentID, source.SHA256)
			err = repo.CompleteIngestDocument(ctx, job.Lease(), now, prepared)
			if tt.wantError {
				if !errors.Is(err, ErrInvalidDocumentUpload) {
					t.Fatalf("CompleteIngestDocument error=%v, want ErrInvalidDocumentUpload", err)
				}
				var status, textState, jobState string
				if scanErr := db.QueryRowContext(ctx, `SELECT d.status,b.text_state,j.state
					FROM kb_documents d
					JOIN kb_semantic_document_bindings b ON b.document_id=d.id
					JOIN kb_knowledge_jobs j ON j.document_id=d.id AND j.kind='ingest'
					WHERE d.id=?`, accepted.DocumentID).Scan(&status, &textState, &jobState); scanErr != nil {
					t.Fatal(scanErr)
				}
				if status != "processing" || textState == string(TextIndexReady) || jobState != string(KnowledgeJobRunning) {
					t.Fatalf("rejected PDF completion leaked publication: status=%s text=%s job=%s",
						status, textState, jobState)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			projection, err := service.GetIngestDocumentProjection(ctx, "desktop-user", accepted.DocumentID)
			if err != nil || projection.TextIndexState != TextIndexReady || projection.PageCount == nil ||
				*projection.PageCount != 3 {
				t.Fatalf("completed projection=%+v err=%v", projection, err)
			}
		})
	}
}

func preparedPDFCompletionDocument(documentID, sourceDigest string) PreparedIngestDocument {
	content := "page 1 content\npage 2 content\npage 3 content"
	now := time.Now().UTC()
	return PreparedIngestDocument{
		Document: &Document{
			ID: documentID, Content: content, Status: "indexed", CreatedAt: now, UpdatedAt: now,
		},
		Chunks: []*Chunk{{
			ID: documentID + "-chunk-0", DocID: documentID, Content: content,
			PageStart: 1, PageEnd: 3, SourceDigest: sourceDigest,
			SourceOffsetStart: 0, SourceOffsetEnd: int64(len(content)),
		}},
		PageCount: 3,
		Warnings:  []string{},
	}
}
