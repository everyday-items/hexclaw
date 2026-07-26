package knowledge

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestDeleteCancelledIngestWithoutSemanticResourcesIsIdempotent(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "cancel before semantic resources exist"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "cancel-then-delete",
		Filename:       "cancelled.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CancelJob(ctx, "desktop-user", accepted.JobID); err != nil {
		t.Fatal(err)
	}

	store := NewSQLiteStore(db, WithSQLiteSemanticMutations("desktop-user", "default"))
	if err := store.Delete(ctx, accepted.DocumentID); err != nil {
		t.Fatalf("first delete cancelled ingest: %v", err)
	}
	if err := store.Delete(ctx, accepted.DocumentID); err != nil {
		t.Fatalf("idempotent replay delete cancelled ingest: %v", err)
	}

	var gcJobs, chunks, revisionRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='gc' AND idempotency_key=?`,
		documentGCIdempotencyPrefix+hex.EncodeToString([]byte(accepted.DocumentID)),
	).Scan(&gcJobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks
		WHERE doc_id=?`, accepted.DocumentID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE document_id=?`, accepted.DocumentID).Scan(&revisionRows); err != nil {
		t.Fatal(err)
	}
	if gcJobs != 1 || chunks != 0 || revisionRows != 0 {
		t.Fatalf("delete projection gc_jobs=%d chunks=%d revision_rows=%d", gcJobs, chunks, revisionRows)
	}
}
