package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type mutableVisionRouteResolver struct {
	mu       sync.Mutex
	snapshot VisionRouteSnapshot
}

func (r *mutableVisionRouteResolver) FreezeDefaultVisionRoute(context.Context) (VisionRouteSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot, nil
}

func (r *mutableVisionRouteResolver) set(snapshot VisionRouteSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshot = snapshot
}

func visionRouteFixture(providerID, providerName, displayName, model string) VisionRouteSnapshot {
	return VisionRouteSnapshot{
		ProviderInstanceID: providerID,
		ProviderName:       providerName,
		ProviderDisplayName: displayName,
		Model:              model,
		Capabilities:       []string{"text", "vision"},
	}
}

func TestCreateAndReplacementRetryFreezeIndependentVisionRoutes(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	if err := migrate.Run(ctx, db, []migrate.Migration{migrate.KnowledgeIngestExecutionV46}); err != nil {
		t.Fatal(err)
	}
	routeA := visionRouteFixture("provider-instance-a", "hexclaw-gpt", "HexClaw-GPT", "gpt-5.6-sol")
	routeB := visionRouteFixture("provider-instance-b", "openrouter", "OpenRouter", "openai/gpt-5.1")
	routeC := visionRouteFixture("provider-instance-c", "ollama", "Ollama", "qwen3.5:9b")
	resolver := &mutableVisionRouteResolver{snapshot: routeA}
	service.ConfigureVisionRouteResolver(resolver)

	body := "immutable route snapshot source"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "vision-route-original",
		Filename:       "route.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVisionRouteSnapshot(t, db, ctx, accepted.JobID, routeA)

	repository := NewSQLiteSemanticIndexRepository(db)
	failed := failNextKnowledgeJob(t, repository, "desktop-user", "default", "route-failure-worker")
	if failed.JobID != accepted.JobID {
		t.Fatalf("failed job=%s want=%s", failed.JobID, accepted.JobID)
	}
	resolver.set(routeB)
	retry, err := service.RetryDocument(
		ctx, "desktop-user", "default", accepted.DocumentID, "vision-route-retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if retry.JobID == accepted.JobID || retry.DocumentID != accepted.DocumentID {
		t.Fatalf("replacement retry=%+v original=%+v", retry, accepted)
	}
	assertVisionRouteSnapshot(t, db, ctx, retry.JobID, routeB)

	resolver.set(routeC)
	replayed, err := service.RetryDocument(
		ctx, "desktop-user", "default", accepted.DocumentID, "vision-route-retry",
	)
	if err != nil || replayed != retry {
		t.Fatalf("idempotent retry replay=%+v err=%v want=%+v", replayed, err, retry)
	}
	assertVisionRouteSnapshot(t, db, ctx, retry.JobID, routeB)
	assertVisionRouteSnapshot(t, db, ctx, accepted.JobID, routeA)

	var generations, sources, blobs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT document_generation)
		FROM kb_knowledge_jobs WHERE job_id IN (?,?)`, accepted.JobID, retry.JobID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=?`, accepted.DocumentID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_blobs b
		JOIN kb_ingest_document_sources s
		  ON s.owner_id=b.owner_id AND s.corpus_uid=b.corpus_uid AND s.blob_sha256=b.sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || sources != 1 || blobs != 1 {
		t.Fatalf("replacement retry generations=%d sources=%d blobs=%d", generations, sources, blobs)
	}
}

func assertVisionRouteSnapshot(
	t *testing.T,
	db queryRower,
	ctx context.Context,
	jobID string,
	want VisionRouteSnapshot,
) {
	t.Helper()
	var got VisionRouteSnapshot
	var capabilitiesJSON string
	if err := db.QueryRowContext(ctx, `SELECT provider_instance_id,provider_name,
		provider_display_name,model,capabilities_json,selection_fingerprint
		FROM kb_ingest_execution_snapshots WHERE job_id=?`, jobID).Scan(
		&got.ProviderInstanceID, &got.ProviderName, &got.ProviderDisplayName,
		&got.Model, &capabilitiesJSON, &got.Fingerprint,
	); err != nil {
		t.Fatal(err)
	}
	if err := got.UnmarshalCapabilitiesJSON(capabilitiesJSON); err != nil {
		t.Fatal(err)
	}
	want = want.Canonical()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("job %s route=%+v want=%+v", jobID, got, want)
	}
}

type queryRower = *sql.DB

func TestAdaptiveIngestSegmentsPreserveParentPagesAndCaps(t *testing.T) {
	pages := make([]IngestPagePlan, 0, 31)
	for page := 1; page <= 23; page++ {
		pages = append(pages, IngestPagePlan{PageNumber: page, Mode: IngestSegmentText})
	}
	for page := 24; page <= 30; page++ {
		pages = append(pages, IngestPagePlan{PageNumber: page, Mode: IngestSegmentVisual})
	}
	pages = append(pages, IngestPagePlan{PageNumber: 31, Mode: IngestSegmentText})

	got, err := PlanAdaptiveIngestSegments(pages, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []IngestSegmentPlan{
		{Ordinal: 1, PageStart: 1, PageEnd: 20, Mode: IngestSegmentText},
		{Ordinal: 2, PageStart: 21, PageEnd: 23, Mode: IngestSegmentText},
		{Ordinal: 3, PageStart: 24, PageEnd: 25, Mode: IngestSegmentVisual},
		{Ordinal: 4, PageStart: 26, PageEnd: 27, Mode: IngestSegmentVisual},
		{Ordinal: 5, PageStart: 28, PageEnd: 29, Mode: IngestSegmentVisual},
		{Ordinal: 6, PageStart: 30, PageEnd: 30, Mode: IngestSegmentVisual},
		{Ordinal: 7, PageStart: 31, PageEnd: 31, Mode: IngestSegmentText},
	}
	if len(got) != len(want) {
		t.Fatalf("segments=%+v want=%+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment[%d]=%+v want=%+v", i, got[i], want[i])
		}
	}

	if _, err := PlanAdaptiveIngestSegments([]IngestPagePlan{
		{PageNumber: 1, Mode: IngestSegmentText},
		{PageNumber: 3, Mode: IngestSegmentText},
	}, 2); !errors.Is(err, ErrInvalidIngestSegmentPlan) {
		t.Fatalf("non-contiguous pages error=%v", err)
	}
}

func TestVisionModelRequiredFailureIsTypedAndDurable(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	if err := migrate.Run(ctx, db, []migrate.Migration{migrate.KnowledgeIngestExecutionV46}); err != nil {
		t.Fatal(err)
	}
	body := "typed vision failure"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "typed-vision-failure",
		Filename:       "vision.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repository.ClaimNextJobForCorpus(
		ctx, "desktop-user", "default", "vision-worker", now, time.Minute,
	)
	if err != nil || !claimed || job.JobID != accepted.JobID {
		t.Fatalf("claim job=%+v claimed=%v err=%v", job, claimed, err)
	}
	route := visionRouteFixture("provider-instance-a", "hexclaw-gpt", "HexClaw-GPT", "gpt-5.6-sol")
	typedErr := NewVisionModelRequiredError(route, []int{2, 4, 7})
	if !errors.Is(typedErr, ErrVisionModelRequired) {
		t.Fatalf("typed error=%v does not wrap ErrVisionModelRequired", typedErr)
	}
	failed, err := repository.FailJobWithFailure(
		ctx, job.Lease(), now.Add(time.Second), KnowledgeJobFailureFromError(typedErr),
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Failure == nil || failed.Failure.Code != VisionModelRequiredFailureCode ||
		len(failed.Failure.AffectedPages) != 3 {
		t.Fatalf("failed job failure=%+v", failed.Failure)
	}
	reloaded, err := service.GetJob(ctx, "desktop-user", failed.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Failure == nil || reloaded.Failure.Code != VisionModelRequiredFailureCode ||
		reloaded.Failure.ProviderDisplayName != "HexClaw-GPT" ||
		reloaded.Failure.Model != "gpt-5.6-sol" {
		t.Fatalf("reloaded failure=%+v", reloaded.Failure)
	}
}
