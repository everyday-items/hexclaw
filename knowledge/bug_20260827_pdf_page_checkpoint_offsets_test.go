package knowledge

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPageCheckpointRepairsMissingCanonicalOffsetsWithoutChangingIdentity(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "%PDF-1.7\npage checkpoint offset repair"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "page-offset-repair",
		Filename:       "offsets.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repo.ClaimNextJobForCorpus(ctx, "desktop-user", "default", "offset-worker", now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	source, err := repo.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	checkpoint := IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "text", Content: "page body",
	}
	if err := repo.SaveIngestPageCheckpoint(ctx, job.Lease(), now, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.SourceOffsetStart = 17
	checkpoint.SourceOffsetEnd = 26
	if err := repo.SaveIngestPageCheckpoint(ctx, job.Lease(), now, checkpoint); err != nil {
		if errors.Is(err, ErrInvalidDocumentUpload) {
			t.Fatalf("missing offsets must be repairable, got immutable conflict: %v", err)
		}
		t.Fatal(err)
	}
	pages, err := repo.LoadIngestPageCheckpoints(ctx, job.Lease(), now, source.SHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].SourceOffsetStart != 17 || pages[0].SourceOffsetEnd != 26 {
		t.Fatalf("repaired checkpoint=%+v", pages)
	}
	// 前序 OCR 页补齐后，后续页在完整 canonical 文本中的派生坐标可以合法移动。
	checkpoint.SourceOffsetStart = 21
	checkpoint.SourceOffsetEnd = 30
	if err := repo.SaveIngestPageCheckpoint(ctx, job.Lease(), now, checkpoint); err != nil {
		t.Fatalf("derived offset refresh must remain idempotent for the same page identity: %v", err)
	}
	pages, err = repo.LoadIngestPageCheckpoints(ctx, job.Lease(), now, source.SHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].SourceOffsetStart != 21 || pages[0].SourceOffsetEnd != 30 {
		t.Fatalf("refreshed checkpoint=%+v", pages)
	}
}
