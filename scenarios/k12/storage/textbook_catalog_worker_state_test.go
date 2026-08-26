package k12storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestREGTextbookCatalog_RetryBackoffAndTerminalFailureAreDurable(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	ctx := context.Background()
	started := time.UnixMilli(10_000)
	claim, found, err := store.ClaimTextbookCatalogJob(ctx, "worker-a", started, 30*time.Second)
	if err != nil || !found || claim.Attempt != 1 {
		t.Fatalf("first claim=%+v found=%v err=%v", claim, found, err)
	}
	if err := store.FailTextbookCatalogJob(ctx, claim, k12storage.TextbookCatalogFailure{
		Code: "temporary", Message: "temporary catalog failure",
		RetryAt: started.Add(20 * time.Second),
	}, started.Add(time.Second)); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if _, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-early", started.Add(19*time.Second), 30*time.Second,
	); err != nil || found {
		t.Fatalf("early retry claim found=%v err=%v", found, err)
	}
	second, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-b", started.Add(21*time.Second), 30*time.Second,
	)
	if err != nil || !found || second.Attempt != 2 || second.LeaseEpoch != 2 {
		t.Fatalf("due retry claim=%+v found=%v err=%v", second, found, err)
	}
	if err := store.FailTextbookCatalogJob(ctx, second, k12storage.TextbookCatalogFailure{
		Code: "evidence_incomplete", Message: "catalog proof incomplete", Terminal: true,
	}, started.Add(22*time.Second)); err != nil {
		t.Fatalf("terminal failure: %v", err)
	}

	// A GET/reconcile pass must consume the durable job state rather than
	// projecting text_state=ready back to extracting.
	if _, err := store.ListTextbookBindingOptions(ctx, k12storage.TextbookScope{
		OwnerID: "desktop-user", AgentName: "mingming", Subject: "math",
	}); err != nil {
		t.Fatal(err)
	}
	var manifestState, jobState string
	if err := store.DB().QueryRow(`SELECT state FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`).Scan(&manifestState); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT state FROM k12_textbook_catalog_jobs
		WHERE job_id='catalog-job'`).Scan(&jobState); err != nil {
		t.Fatal(err)
	}
	if manifestState != "failed_terminal" || jobState != "failed_terminal" {
		t.Fatalf("GET revived terminal failure: manifest/job=%s/%s", manifestState, jobState)
	}
}

func TestREGTextbookCatalog_HeartbeatExtendsLeaseAndFencesSteal(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	ctx := context.Background()
	started := time.UnixMilli(10_000)
	claim, found, err := store.ClaimTextbookCatalogJob(ctx, "worker-a", started, 30*time.Second)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := store.RenewTextbookCatalogJob(
		ctx, claim, started.Add(20*time.Second), 30*time.Second,
	); err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	if _, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-early", started.Add(31*time.Second), 30*time.Second,
	); err != nil || found {
		t.Fatalf("heartbeat did not prevent steal: found=%v err=%v", found, err)
	}
	newClaim, found, err := store.ClaimTextbookCatalogJob(
		ctx, "worker-new", started.Add(51*time.Second), 30*time.Second,
	)
	if err != nil || !found || newClaim.LeaseEpoch != claim.LeaseEpoch+1 {
		t.Fatalf("expired renewed lease not recoverable: %+v found=%v err=%v", newClaim, found, err)
	}
	if err := store.RenewTextbookCatalogJob(
		ctx, claim, started.Add(52*time.Second), 30*time.Second,
	); !errors.Is(err, k12storage.ErrTextbookCatalogJobFenced) {
		t.Fatalf("old heartbeat error=%v want fenced", err)
	}
}

func TestREGTextbookCatalog_RecoveryPinsExactIngestSnapshotAndDetectsDrift(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	if _, err := db.Exec(`DELETE FROM k12_textbook_catalog_jobs
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	if err := store.RecoverTextbookCatalogJobs(ctx, now, 8); err != nil {
		t.Fatalf("recover missed catalog job: %v", err)
	}
	claim, found, err := store.ClaimTextbookCatalogJob(ctx, "worker", now, 30*time.Second)
	if err != nil || !found || claim.IngestJobID != "catalog-ingest" ||
		len(claim.SourcePlanDigest) != 64 {
		t.Fatalf("pinned claim=%+v found=%v err=%v", claim, found, err)
	}
	source, err := store.LoadTextbookCatalogSource(ctx, claim, now.Add(time.Second))
	if err != nil {
		t.Fatalf("load pinned source: %v", err)
	}
	if len(source.Pages) != 3 || source.Pages[2].PDFPage != 3 ||
		len(source.Pages[2].SegmentRefs) != 1 || source.Pages[2].SegmentRefs[0] != "catalog-segment-3" {
		t.Fatalf("unexpected pinned source: %+v", source)
	}
	if _, err := db.Exec(`UPDATE kb_ingest_page_checkpoints SET content='tampered'
		WHERE job_id='catalog-ingest' AND page_number=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTextbookCatalogSource(ctx, claim, now.Add(2*time.Second)); !errors.Is(err, k12storage.ErrTextbookCatalogSourceIncomplete) {
		t.Fatalf("source drift error=%v want incomplete/fail-closed", err)
	}
}

func TestREGTextbookCatalog_AcceptsCompletedPageFactsFromPriorIngestLease(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	// 任务成功收口会递增任务 epoch；已完成页仍保留生成它们的旧租约 epoch。
	if _, err := db.Exec(`UPDATE kb_knowledge_jobs
		SET lease_epoch=2 WHERE job_id='catalog-ingest'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM k12_textbook_catalog_jobs
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	if err := store.RecoverTextbookCatalogJobs(ctx, now, 8); err != nil {
		t.Fatalf("recover prior-lease page facts: %v", err)
	}
	var state, ingestJobID, sourcePlanDigest string
	if err := db.QueryRow(`SELECT state,ingest_job_id,source_plan_digest
		FROM k12_textbook_catalog_jobs WHERE manifest_id='catalog-manifest'`).Scan(
		&state, &ingestJobID, &sourcePlanDigest,
	); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || ingestJobID != "catalog-ingest" || len(sourcePlanDigest) != 64 {
		t.Fatalf("recovered catalog state=%s ingest=%s plan=%q", state, ingestJobID, sourcePlanDigest)
	}
	claim, found, err := store.ClaimTextbookCatalogJob(ctx, "worker", now, 30*time.Second)
	if err != nil || !found || claim.IngestJobID != "catalog-ingest" ||
		claim.SourcePlanDigest != sourcePlanDigest {
		t.Fatalf("claim prior-lease source=%+v found=%v err=%v", claim, found, err)
	}
	if _, err := store.LoadTextbookCatalogSource(ctx, claim, now.Add(time.Second)); err != nil {
		t.Fatalf("load prior-lease source: %v", err)
	}
	if _, err := db.Exec(`UPDATE kb_ingest_page_checkpoints
		SET lease_epoch=3 WHERE job_id='catalog-ingest'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTextbookCatalogSource(ctx, claim, now.Add(2*time.Second)); !errors.Is(err, k12storage.ErrTextbookCatalogSourceIncomplete) {
		t.Fatalf("future-lease page facts error=%v want incomplete", err)
	}
}

func TestREGTextbookCatalog_ReopensOnlySourceEvidenceTerminalAfterFactsComplete(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	db := store.DB()
	if _, err := db.Exec(`UPDATE kb_knowledge_jobs
		SET lease_epoch=2 WHERE job_id='catalog-ingest'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_textbook_catalog_jobs
		SET state='failed_terminal',failure_code='source_evidence_incomplete',
		    ingest_job_id='catalog-ingest',source_plan_digest='',last_error='识别失败'
		WHERE job_id='catalog-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_textbook_manifests
		SET state='failed_terminal',retryable=0,failure_message='识别失败'
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverTextbookCatalogJobs(context.Background(), time.UnixMilli(10_000), 8); err != nil {
		t.Fatalf("recover source-evidence terminal: %v", err)
	}
	var state, manifestState, ingestJobID, sourcePlanDigest string
	if err := db.QueryRow(`SELECT state,ingest_job_id,source_plan_digest
		FROM k12_textbook_catalog_jobs WHERE job_id='catalog-job'`).Scan(
		&state, &ingestJobID, &sourcePlanDigest,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`).Scan(&manifestState); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || manifestState != "extracting" || ingestJobID != "catalog-ingest" ||
		len(sourcePlanDigest) != 64 {
		t.Fatalf("reopened source-evidence terminal job=%s manifest=%s ingest=%s plan=%q",
			state, manifestState, ingestJobID, sourcePlanDigest)
	}
	if _, err := db.Exec(`UPDATE k12_textbook_catalog_jobs
		SET state='failed_terminal',failure_code='evidence_incomplete',
		    last_error='catalog proof incomplete'
		WHERE job_id='catalog-job'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverTextbookCatalogJobs(context.Background(), time.UnixMilli(11_000), 8); err != nil {
		t.Fatalf("recover unrelated terminal: %v", err)
	}
	if err := db.QueryRow(`SELECT state FROM k12_textbook_catalog_jobs
		WHERE job_id='catalog-job'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed_terminal" {
		t.Fatalf("unrelated terminal state=%s want failed_terminal", state)
	}
}
