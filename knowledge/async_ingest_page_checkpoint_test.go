package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type restartablePageProcessor struct {
	calls    map[int]int
	failOnce bool
}

func (p *restartablePageProcessor) Prepare(
	context.Context,
	PersistedIngestDocument,
) (PreparedIngestDocument, error) {
	return PreparedIngestDocument{}, errors.New("legacy prepare path must not be used")
}

func (p *restartablePageProcessor) PrepareResumable(
	ctx context.Context,
	source PersistedIngestDocument,
	progress IngestPageProgress,
) (PreparedIngestDocument, error) {
	const total = int64(3)
	if err := progress.SetPageTotal(ctx, source.SHA256, total); err != nil {
		return PreparedIngestDocument{}, err
	}
	completed, err := progress.LoadCompletedPages(ctx, source.SHA256, total)
	if err != nil {
		return PreparedIngestDocument{}, err
	}
	byPage := make(map[int]IngestPageCheckpoint, len(completed))
	for _, page := range completed {
		byPage[page.PageNumber] = page
	}
	for page := 1; page <= int(total); page++ {
		if _, ok := byPage[page]; ok {
			continue
		}
		p.calls[page]++
		text := fmt.Sprintf("page %d restart-safe lesson", page)
		checkpoint := IngestPageCheckpoint{
			PageNumber: page, PagesTotal: total, SourceDigest: source.SHA256,
			ExtractionMode: "ocr_vlm", Content: text,
		}
		if err := progress.CommitPage(ctx, checkpoint); err != nil {
			return PreparedIngestDocument{}, err
		}
		byPage[page] = checkpoint
		if p.failOnce && page == 2 {
			p.failOnce = false
			return PreparedIngestDocument{}, errors.New("temporary VLM transport failure")
		}
	}
	parts := make([]string, 0, total)
	for page := 1; page <= int(total); page++ {
		parts = append(parts, byPage[page].Content)
	}
	content := strings.Join(parts, "\n")
	now := time.Now().UTC()
	doc := &Document{
		ID: source.DocumentID, Title: source.Filename, Content: content,
		Source: "upload:" + source.Filename, SourceType: "upload", Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	return PreparedIngestDocument{
		Document: doc,
		Chunks: []*Chunk{{
			ID: doc.ID + "-chunk-0", DocID: doc.ID, Content: content,
			Index: 0, PageStart: 1, PageEnd: 3, SourceDigest: source.SHA256,
			SourceOffsetStart: 0, SourceOffsetEnd: int64(len(content)),
		}},
		PageCount: total,
	}, nil
}

func TestIngestPageCheckpointResumesWithoutRepeatingCompletedVLM(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "%PDF-1.7\ndurable scanned pdf bytes"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "page-checkpoint-restart", Filename: "scan.pdf", MediaType: "application/pdf",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Now().UTC()
	processor := &restartablePageProcessor{calls: map[int]int{}, failOnce: true}
	worker := NewSemanticIndexWorker(NewSQLiteSemanticIndexRepository(db), nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "restart-worker",
		LeaseDuration: time.Minute, RetryDelay: time.Second, Now: func() time.Time { return clock },
	})
	worker.SetDocumentIngestProcessor(processor)
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first RunOnce worked=%v err=%v", worked, err)
	}
	first, err := service.GetJob(ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if first.PagesDone == nil || first.PagesTotal == nil || *first.PagesDone != 2 || *first.PagesTotal != 3 {
		t.Fatalf("durable progress after interruption=%+v", first)
	}

	clock = clock.Add(2 * time.Second)
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("resumed RunOnce worked=%v err=%v", worked, err)
	}
	if processor.calls[1] != 1 || processor.calls[2] != 1 || processor.calls[3] != 1 {
		t.Fatalf("VLM calls=%v; completed pages were repeated", processor.calls)
	}
	final, err := service.GetJob(ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != KnowledgeJobSucceeded || final.PagesDone == nil || *final.PagesDone != 3 {
		t.Fatalf("final job=%+v", final)
	}
}

func TestIngestPageCheckpointRejectsStaleLease(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "%PDF-1.7\ndurable source"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "page-checkpoint-fence", Filename: "scan.pdf", MediaType: "application/pdf",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	oldJob, claimed, err := repo.ClaimNextJobForCorpus(ctx, "desktop-user", "default", "old-worker", now, time.Second)
	if err != nil || !claimed {
		t.Fatalf("old claim=%v err=%v", claimed, err)
	}
	source, err := repo.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetIngestPageTotal(ctx, oldJob.Lease(), now, source.SHA256, 2); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveIngestPageCheckpoint(ctx, oldJob.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 2, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "first page",
	}); err != nil {
		t.Fatal(err)
	}

	stealAt := now.Add(2 * time.Second)
	newJob, claimed, err := repo.ClaimNextJobForCorpus(ctx, "desktop-user", "default", "new-worker", stealAt, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("new claim=%v err=%v", claimed, err)
	}
	err = repo.SaveIngestPageCheckpoint(ctx, oldJob.Lease(), stealAt, IngestPageCheckpoint{
		PageNumber: 2, PagesTotal: 2, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "stale second page",
	})
	if !errors.Is(err, ErrJobFenced) {
		t.Fatalf("stale checkpoint error=%v, want ErrJobFenced", err)
	}
	pages, err := repo.LoadIngestPageCheckpoints(ctx, newJob.Lease(), stealAt, source.SHA256, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].PageNumber != 1 {
		t.Fatalf("checkpoints after stale write=%+v", pages)
	}
}
