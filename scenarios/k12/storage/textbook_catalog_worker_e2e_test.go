package k12storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestREGTextbookCatalog_ProductionWorkerPublishesPersistedCheckpointCatalog(t *testing.T) {
	store, _, _ := seedTextbookCatalogMaterialization(t)
	if _, err := store.DB().Exec(`DELETE FROM k12_textbook_catalog_jobs
		WHERE manifest_id='catalog-manifest'`); err != nil {
		t.Fatal(err)
	}
	worker := usecase.NewTextbookCatalogWorker(
		store,
		usecase.TextbookCatalogCheckpointExtractor{},
		usecase.TextbookCatalogWorkerConfig{
			WorkerID: "integration-worker", Lease: 30 * time.Second,
			HeartbeatInterval: 10 * time.Second, ExtractTimeout: time.Second,
			MaxAttempts: 3, RetryBase: time.Second, RetryMax: time.Minute,
			RecoveryBatch: 8,
		},
	)
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("production worker processed=%v err=%v", processed, err)
	}
	var manifestState, jobState string
	var logicalPage, pdfPage int
	if err := store.DB().QueryRow(`SELECT state FROM k12_textbook_manifests
		WHERE manifest_id='catalog-manifest'`).Scan(&manifestState); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT state FROM k12_textbook_catalog_jobs
		WHERE manifest_id='catalog-manifest'`).Scan(&jobState); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT logical_page,pdf_page
		FROM k12_textbook_page_mappings WHERE manifest_id='catalog-manifest'`).Scan(
		&logicalPage, &pdfPage,
	); err != nil {
		t.Fatal(err)
	}
	if manifestState != "ready_for_confirmation" || jobState != "succeeded" ||
		logicalPage != 1 || pdfPage != 3 {
		t.Fatalf("closed loop states/map=%s/%s %d->%d",
			manifestState, jobState, logicalPage, pdfPage)
	}
}
