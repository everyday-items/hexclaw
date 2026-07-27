package knowledge

import (
	"strings"
	"testing"
	"time"
)

func TestBUG20260726025_InternalSegmentsStayBehindSingleLogicalDocument(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "%PDF-1.7\nsingle logical textbook fixture"
	accepted, err := service.CreateDocument(
		ctx,
		"desktop-user",
		"default",
		CreateDocumentInput{
			IdempotencyKey: "bug-20260726-025-single-logical-document",
			Filename:       "textbook-100-pages.pdf",
			MediaType:      "application/pdf",
			SizeBytes:      int64(len(body)),
			Body:           strings.NewReader(body),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	repository := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repository.ClaimNextJobForCorpus(
		ctx,
		"desktop-user",
		"default",
		"bug-20260726-025-worker",
		now,
		time.Minute,
	)
	if err != nil || !claimed || job.JobID != accepted.JobID {
		t.Fatalf("claim job=%+v claimed=%v err=%v", job, claimed, err)
	}
	source, err := repository.GetIngestDocument(
		ctx,
		"desktop-user",
		accepted.DocumentID,
	)
	if err != nil {
		t.Fatal(err)
	}

	pages := make([]IngestPagePlan, 0, 45)
	for page := 1; page <= 45; page++ {
		pages = append(pages, IngestPagePlan{
			PageNumber: page,
			Mode:       IngestSegmentText,
		})
	}
	segments, err := PlanAdaptiveIngestSegments(pages, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) <= 1 {
		t.Fatalf("fixture did not produce multiple internal segments: %+v", segments)
	}
	if err := repository.SetIngestPageTotal(
		ctx,
		job.Lease(),
		now,
		source.SHA256,
		int64(len(pages)),
	); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveIngestSegmentPlan(
		ctx,
		job.Lease(),
		now,
		source.SHA256,
		segments,
	); err != nil {
		t.Fatal(err)
	}

	store := NewSQLiteStore(db)
	manager := NewManager(store, store, nil)
	listed, err := manager.ListDocumentsPaged(ctx, DocListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Documents) != 1 {
		t.Fatalf(
			"BUG-20260726-025: %d internal segments changed the public document count: total=%d documents=%d",
			len(segments),
			listed.Total,
			len(listed.Documents),
		)
	}
	document := listed.Documents[0]
	if document.ID != accepted.DocumentID ||
		document.Title != "textbook-100-pages.pdf" {
		t.Fatalf(
			"BUG-20260726-025: public document projection=%+v want id=%q original title",
			document,
			accepted.DocumentID,
		)
	}

	var storedSegments, distinctSegmentDocuments int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*),COUNT(DISTINCT document_id)
		 FROM kb_ingest_segments WHERE job_id=?`,
		accepted.JobID,
	).Scan(&storedSegments, &distinctSegmentDocuments); err != nil {
		t.Fatal(err)
	}
	if storedSegments != len(segments) || distinctSegmentDocuments != 1 {
		t.Fatalf(
			"internal segment ledger rows/documents=%d/%d want=%d/1",
			storedSegments,
			distinctSegmentDocuments,
			len(segments),
		)
	}
}
